/* SPDX-License-Identifier: GPL-2.0-only */

#include "orbi_bt_setup.h"

#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <stdbool.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <time.h>
#include <unistd.h>

#ifndef AF_BLUETOOTH
#define AF_BLUETOOTH 31
#endif

#define BTPROTO_HCI 1
#define SOL_HCI 0
#define HCI_FILTER 2
#define HCI_CHANNEL_RAW 0
#define HCI_COMMAND_PACKET 0x01
#define HCI_EVENT_PACKET 0x04
#define HCI_VENDOR_EVENT 0xff
#define CSR_OGF 0x3f
#define CSR_OCF 0x0000
#define CSR_OPCODE ((CSR_OCF & 0x03ffU) | (CSR_OGF << 10))
#define CSR_PS_VARID 0x7003
#define CSR_WARM_RESET_VARID 0x4002
#define CSR_ANA_KEY 0x01fe
#define CSR_PSRAM_STORE 8
#define CSR_ANA_FREQUENCY 26000
#define CSR_TIMEOUT_MS 5000

struct sockaddr_hci_min {
	sa_family_t family;
	unsigned short device;
	unsigned short channel;
};

struct hci_filter_min {
	uint32_t type_mask;
	uint32_t event_mask[2];
	uint16_t opcode;
};

static uint16_t get_le16(const uint8_t *data)
{
	return (uint16_t) data[0] | ((uint16_t) data[1] << 8);
}

static void put_le16(uint8_t *data, uint16_t value)
{
	data[0] = (uint8_t) (value & 0xffU);
	data[1] = (uint8_t) (value >> 8);
}

static int hci_index(const char *name, unsigned short *index,
		     struct orbi_error *error)
{
	char *end = NULL;
	unsigned long value;

	if (strncmp(name, "hci", 3) != 0 || name[3] == '\0') {
		orbi_error_set(error, "invalid HCI controller identity");
		return -1;
	}
	errno = 0;
	value = strtoul(name + 3, &end, 10);
	if (errno != 0 || end == name + 3 || *end != '\0' || value > 0xffffUL) {
		orbi_error_set(error, "invalid HCI controller identity");
		return -1;
	}
	*index = (unsigned short) value;
	return 0;
}

static int64_t monotonic_milliseconds(struct orbi_error *error)
{
	struct timespec now;

	if (clock_gettime(CLOCK_MONOTONIC, &now) < 0) {
		orbi_error_set(error, "read monotonic clock: %s", strerror(errno));
		return -1;
	}
	return (int64_t) now.tv_sec * 1000 + now.tv_nsec / 1000000;
}

static int wait_fd(int fd, short events, int64_t deadline,
		   struct orbi_error *error)
{
	struct pollfd descriptor = {
		.fd = fd,
		.events = events,
	};

	for (;;) {
		int64_t now = monotonic_milliseconds(error);
		int64_t remaining;
		int timeout;
		int result;

		if (now < 0)
			return -1;
		remaining = deadline - now;
		if (remaining <= 0)
			return -ETIMEDOUT;
		timeout = remaining > INT_MAX ? INT_MAX : (int) remaining;
		result = poll(&descriptor, 1, timeout);
		if (result < 0 && errno == EINTR)
			continue;
		if (result < 0) {
			orbi_error_set(error, "poll HCI socket: %s", strerror(errno));
			return -1;
		}
		if (result == 0)
			return -ETIMEDOUT;
		if ((descriptor.revents & (POLLERR | POLLHUP | POLLNVAL)) != 0) {
			orbi_error_set(error, "HCI socket closed while waiting");
			return -1;
		}
		if ((descriptor.revents & events) != 0)
			return 0;
	}
}

bool orbi_csr_header_matches(const uint8_t *event, size_t event_length,
			     const uint8_t *pdu, size_t pdu_length)
{
	return event_length >= 9 && pdu_length >= 9 && event[0] == 0xc2 &&
	       get_le16(event + 5) == get_le16(pdu + 5) &&
	       get_le16(event + 7) == get_le16(pdu + 7);
}

static int process_vendor_packet(const uint8_t *packet, size_t packet_length,
				 const uint8_t *pdu, size_t pdu_length,
				 uint8_t *event, size_t event_capacity,
				 size_t *event_length,
				 struct orbi_error *error)
{
	size_t payload_length;

	if (packet_length < 3 || packet[0] != HCI_EVENT_PACKET ||
	    packet[1] != HCI_VENDOR_EVENT) {
		orbi_error_set(error, "raw HCI returned an unexpected packet");
		return -1;
	}
	payload_length = packet[2];
	if (packet_length != payload_length + 3) {
		orbi_error_set(error, "raw HCI vendor event length mismatch");
		return -1;
	}
	if (!orbi_csr_header_matches(packet + 3, payload_length,
				     pdu, pdu_length))
		return 1;
	if (payload_length > event_capacity) {
		orbi_error_set(error, "CSR response buffer is too small");
		return -1;
	}
	memcpy(event, packet + 3, payload_length);
	*event_length = payload_length;
	return 0;
}

#ifdef ORBI_BT_TESTING
int orbi_test_process_vendor_packet(const uint8_t *packet,
				    size_t packet_length,
				    const uint8_t *pdu,
				    size_t pdu_length,
				    uint8_t *event,
				    size_t *event_length,
				    struct orbi_error *error)
{
	size_t capacity = *event_length;

	*event_length = 0;
	return process_vendor_packet(packet, packet_length, pdu, pdu_length,
				     event, capacity, event_length, error);
}
#endif

int orbi_csr_command_raw(const char *hci, const uint8_t *pdu,
			 size_t pdu_length, uint8_t *event,
			 size_t *event_length, bool *dispatched,
			 unsigned int timeout_ms, void *opaque,
			 struct orbi_error *error)
{
	struct sockaddr_hci_min address;
	struct hci_filter_min filter;
	uint8_t packet[4 + ORBI_CSR_EVENT_MAX];
	uint8_t incoming[3 + ORBI_CSR_EVENT_MAX];
	unsigned short index;
	size_t event_capacity = *event_length;
	int64_t start;
	int64_t deadline;
	int socket_fd = -1;
	int result = -1;

	(void) opaque;
	*dispatched = false;
	*event_length = 0;
	if (pdu_length == 0 || pdu_length > ORBI_CSR_EVENT_MAX) {
		orbi_error_set(error, "invalid CSR command length");
		return -1;
	}
	if (hci_index(hci, &index, error) < 0)
		return -1;
	socket_fd = socket(AF_BLUETOOTH,
			   SOCK_RAW | SOCK_CLOEXEC | SOCK_NONBLOCK,
			   BTPROTO_HCI);
	if (socket_fd < 0) {
		orbi_error_set(error, "open raw HCI socket: %s", strerror(errno));
		return -1;
	}
	memset(&address, 0, sizeof(address));
	address.family = AF_BLUETOOTH;
	address.device = index;
	address.channel = HCI_CHANNEL_RAW;
	if (bind(socket_fd, (const struct sockaddr *) &address,
		 sizeof(address)) < 0) {
		orbi_error_set(error, "bind raw HCI socket: %s", strerror(errno));
		goto out;
	}
	memset(&filter, 0, sizeof(filter));
	filter.type_mask = 1U << HCI_EVENT_PACKET;
	filter.event_mask[1] = 1U << 31;
	if (setsockopt(socket_fd, SOL_HCI, HCI_FILTER,
		       &filter, sizeof(filter)) < 0) {
		orbi_error_set(error, "set raw HCI filter: %s", strerror(errno));
		goto out;
	}
	start = monotonic_milliseconds(error);
	if (start < 0)
		goto out;
	deadline = start + (int64_t) timeout_ms;
	packet[0] = HCI_COMMAND_PACKET;
	put_le16(packet + 1, CSR_OPCODE);
	packet[3] = (uint8_t) pdu_length;
	memcpy(packet + 4, pdu, pdu_length);
	result = wait_fd(socket_fd, POLLOUT, deadline, error);
	if (result < 0)
		goto out;
	for (;;) {
		ssize_t written = write(socket_fd, packet, pdu_length + 4);

		if (written < 0 && (errno == EINTR || errno == EAGAIN)) {
			result = wait_fd(socket_fd, POLLOUT, deadline, error);
			if (result < 0)
				goto out;
			continue;
		}
		if (written < 0) {
			orbi_error_set(error, "dispatch CSR command: %s",
				       strerror(errno));
			goto out;
		}
		if ((size_t) written != pdu_length + 4) {
			orbi_error_set(error,
				       "raw HCI socket made a partial command write");
			goto out;
		}
		break;
	}
	*dispatched = true;
	for (;;) {
		ssize_t length;

		result = wait_fd(socket_fd, POLLIN, deadline, error);
		if (result == -ETIMEDOUT) {
			orbi_error_set(error, "CSR response timed out");
			goto out;
		}
		if (result < 0)
			goto out;
		length = read(socket_fd, incoming, sizeof(incoming));
		if (length < 0 && (errno == EINTR || errno == EAGAIN))
			continue;
		if (length < 0) {
			orbi_error_set(error, "read CSR response: %s",
				       strerror(errno));
			goto out;
		}
		result = process_vendor_packet(incoming, (size_t) length,
					       pdu, pdu_length, event,
					       event_capacity, event_length,
					       error);
		if (result > 0)
			continue;
		if (result < 0)
			goto out;
		result = 0;
		goto out;
	}

out:
	if (socket_fd >= 0)
		close(socket_fd);
	return result;
}

static void build_pdu(uint8_t *pdu, const uint16_t words[9])
{
	size_t index;

	pdu[0] = 0xc2;
	for (index = 0; index < 9; index++)
		put_le16(pdu + 1 + index * 2, words[index]);
}

static int response_words(const uint8_t *event, size_t event_length,
			  uint16_t words[9], struct orbi_error *error)
{
	size_t index;

	if (event_length != ORBI_CSR_PDU_SIZE || event[0] != 0xc2) {
		orbi_error_set(error, "CSR response has an invalid shape");
		return -1;
	}
	for (index = 0; index < 9; index++)
		words[index] = get_le16(event + 1 + index * 2);
	return 0;
}

static bool ana_response_matches(const uint16_t words[9], uint16_t sequence)
{
	return words[0] == 1 && words[1] == 9 && words[2] == sequence &&
	       words[3] == CSR_PS_VARID && words[5] == CSR_ANA_KEY &&
	       words[6] == 1 && words[7] == CSR_PSRAM_STORE;
}

static int run_csr(const char *hci, const uint8_t *pdu,
		   uint8_t *event, size_t *event_length, bool *dispatched,
		   const struct setup_ops *ops, struct orbi_error *error)
{
	if (ops == NULL || ops->csr_command == NULL) {
		orbi_error_set(error, "CSR command operation is unavailable");
		return -1;
	}
	return ops->csr_command(hci, pdu, ORBI_CSR_PDU_SIZE,
				event, event_length, dispatched,
				CSR_TIMEOUT_MS, ops->opaque, error);
}

int orbi_csr_read_ana(const char *hci, uint16_t sequence,
		      uint16_t *status, uint16_t *value,
		      const struct setup_ops *ops, struct orbi_error *error)
{
	const uint16_t request[9] = {
		0, 9, sequence, CSR_PS_VARID, 0,
		CSR_ANA_KEY, 1, CSR_PSRAM_STORE, 0,
	};
	uint16_t response[9];
	uint8_t pdu[ORBI_CSR_PDU_SIZE];
	uint8_t event[ORBI_CSR_EVENT_MAX];
	size_t event_length = sizeof(event);
	bool dispatched;
	int result;

	build_pdu(pdu, request);
	result = run_csr(hci, pdu, event, &event_length,
			 &dispatched, ops, error);
	if (result < 0 || !dispatched) {
		if (result >= 0)
			orbi_error_set(error, "CSR GET was not dispatched");
		return -1;
	}
	if (response_words(event, event_length, response, error) < 0 ||
	    !ana_response_matches(response, sequence)) {
		if (error->message[0] == '\0')
			orbi_error_set(error,
				       "CSR ANA_FREQ response identity mismatch");
		return -1;
	}
	*status = response[4];
	*value = response[8];
	return 0;
}

int orbi_csr_warm_reset(const char *hci, uint16_t sequence,
			const struct setup_ops *ops, struct orbi_error *error)
{
	const uint16_t request[9] = {
		2, 9, sequence, CSR_WARM_RESET_VARID, 0, 0, 0, 0, 0,
	};
	uint8_t pdu[ORBI_CSR_PDU_SIZE];
	uint8_t event[ORBI_CSR_EVENT_MAX];
	size_t event_length = sizeof(event);
	bool dispatched;
	int result;

	build_pdu(pdu, request);
	result = run_csr(hci, pdu, event, &event_length,
			 &dispatched, ops, error);
	if (!dispatched) {
		if (error->message[0] == '\0')
			orbi_error_set(error, "CSR warm reset was not dispatched");
		return -1;
	}
	if (result == -ETIMEDOUT)
		return 0;
	if (result < 0)
		return -1;
	return 0;
}

int orbi_csr_set_ana(const char *hci, uint16_t sequence,
		     const struct setup_ops *ops, struct orbi_error *error)
{
	const uint16_t request[9] = {
		2, 9, sequence, CSR_PS_VARID, 0,
		CSR_ANA_KEY, 1, CSR_PSRAM_STORE, CSR_ANA_FREQUENCY,
	};
	uint16_t response[9];
	uint16_t status;
	uint16_t value;
	uint8_t pdu[ORBI_CSR_PDU_SIZE];
	uint8_t event[ORBI_CSR_EVENT_MAX];
	size_t event_length = sizeof(event);
	bool dispatched;
	int result;

	build_pdu(pdu, request);
	result = run_csr(hci, pdu, event, &event_length,
			 &dispatched, ops, error);
	if (result < 0 || !dispatched) {
		if (result >= 0)
			orbi_error_set(error, "CSR SET was not dispatched");
		return -1;
	}
	if (response_words(event, event_length, response, error) < 0 ||
	    !ana_response_matches(response, sequence) ||
	    response[4] != 0 || response[8] != CSR_ANA_FREQUENCY) {
		if (error->message[0] == '\0')
			orbi_error_set(error,
				       "CSR ANA_FREQ SET response mismatch");
		return -1;
	}
	if (orbi_csr_read_ana(hci, (uint16_t) (sequence + 1),
			      &status, &value, ops, error) < 0)
		return -1;
	if (status != 0 || value != CSR_ANA_FREQUENCY) {
		orbi_error_set(error,
			       "CSR ANA_FREQ readback is status=%u value=%u",
			       status, value);
		return -1;
	}
	return orbi_csr_warm_reset(hci, (uint16_t) (sequence + 2),
				   ops, error);
}

int orbi_require_hciextn(const char *path, struct orbi_error *error)
{
	char value[8];
	ssize_t length;
	int fd;

	fd = open(path, O_RDONLY | O_CLOEXEC | O_NOFOLLOW);
	if (fd < 0) {
		orbi_error_set(error, "open hciextn state: %s", strerror(errno));
		return -1;
	}
	do {
		length = read(fd, value, sizeof(value) - 1);
	} while (length < 0 && errno == EINTR);
	if (length < 0) {
		orbi_error_set(error, "read hciextn state: %s", strerror(errno));
		close(fd);
		return -1;
	}
	if (close(fd) < 0) {
		orbi_error_set(error, "close hciextn state: %s", strerror(errno));
		return -1;
	}
	value[length] = '\0';
	while (length > 0 &&
	       (value[length - 1] == '\n' || value[length - 1] == '\r' ||
		value[length - 1] == ' ' || value[length - 1] == '\t'))
		value[--length] = '\0';
	if (strcmp(value, "Y") != 0) {
		orbi_error_set(error, "HCI vendor extensions are not enabled");
		return -1;
	}
	return 0;
}

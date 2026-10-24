/* SPDX-License-Identifier: GPL-2.0-only */

#include "orbi_bt_setup.h"

#include <endian.h>
#include <errno.h>
#include <fcntl.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/sysmacros.h>
#include <sys/wait.h>
#include <unistd.h>

static unsigned int tests_run;
static unsigned int tests_failed;

#define CHECK(condition) do { \
	tests_run++; \
	if (!(condition)) { \
		fprintf(stderr, "%s:%d: check failed: %s\n", \
			__FILE__, __LINE__, #condition); \
		tests_failed++; \
		return; \
	} \
} while (0)

struct fixture {
	char root[PATH_MAX];
	char sysfs[PATH_MAX];
	char dev[PATH_MAX];
	char proc[PATH_MAX];
	char run_dir[PATH_MAX];
	char pending[PATH_MAX];
	char lock[PATH_MAX];
	char hciextn[PATH_MAX];
	char node[PATH_MAX];
	char platform[PATH_MAX];
	char port[PATH_MAX];
	char tty[PATH_MAX];
	struct orbi_paths paths;
};

static int make_path(char *output, size_t capacity,
		     const char *left, const char *right)
{
	char left_copy[PATH_MAX];
	int length;

	if (snprintf(left_copy, sizeof(left_copy), "%s", left) >=
	    (int) sizeof(left_copy))
		return -1;
	length = snprintf(output, capacity, "%s/%s", left_copy, right);

	return length >= 0 && (size_t) length < capacity ? 0 : -1;
}

static int mkdir_p(const char *path)
{
	char copy[PATH_MAX];
	char *cursor;

	if (snprintf(copy, sizeof(copy), "%s", path) >= (int) sizeof(copy))
		return -1;
	for (cursor = copy + 1; *cursor != '\0'; cursor++) {
		if (*cursor != '/')
			continue;
		*cursor = '\0';
		if (mkdir(copy, 0700) < 0 && errno != EEXIST)
			return -1;
		*cursor = '/';
	}
	return mkdir(copy, 0700) == 0 || errno == EEXIST ? 0 : -1;
}

static int write_data(const char *path, const void *data, size_t length)
{
	char parent[PATH_MAX];
	char *slash;
	size_t offset = 0;
	int fd;

	if (snprintf(parent, sizeof(parent), "%s", path) >= (int) sizeof(parent))
		return -1;
	slash = strrchr(parent, '/');
	if (slash == NULL)
		return -1;
	*slash = '\0';
	if (mkdir_p(parent) < 0)
		return -1;
	fd = open(path, O_CREAT | O_TRUNC | O_WRONLY | O_CLOEXEC, 0600);
	if (fd < 0)
		return -1;
	while (offset < length) {
		ssize_t written = write(fd, (const uint8_t *) data + offset,
					length - offset);

		if (written < 0 && errno == EINTR)
			continue;
		if (written <= 0) {
			close(fd);
			return -1;
		}
		offset += (size_t) written;
	}
	return close(fd);
}

static int write_text(const char *path, const char *text)
{
	return write_data(path, text, strlen(text));
}

static int make_link(const char *target, const char *path)
{
	char parent[PATH_MAX];
	char *slash;

	if (snprintf(parent, sizeof(parent), "%s", path) >= (int) sizeof(parent))
		return -1;
	slash = strrchr(parent, '/');
	if (slash == NULL)
		return -1;
	*slash = '\0';
	if (mkdir_p(parent) < 0)
		return -1;
	return symlink(target, path);
}

static int replace_link(const char *target, const char *path)
{
	if (unlink(path) < 0)
		return -1;
	return make_link(target, path);
}

static int remove_tree(const char *path)
{
	pid_t child = fork();
	int status;

	if (child < 0)
		return -1;
	if (child == 0) {
		execl("/bin/rm", "rm", "-rf", "--", path, (char *) NULL);
		_exit(127);
	}
	while (waitpid(child, &status, 0) < 0) {
		if (errno != EINTR)
			return -1;
	}
	return WIFEXITED(status) && WEXITSTATUS(status) == 0 ? 0 : -1;
}

static int fixture_init(struct fixture *fixture, const char *board, bool nested)
{
	char template[] = "/tmp/orbi-bt-c-test-XXXXXX";
	char path[PATH_MAX];
	char ctrl[PATH_MAX];
	char driver[PATH_MAX];
	char data[128];
	uint32_t registers[2] = {
		htobe32(0x078b0000U),
		htobe32(0x200U),
	};

	memset(fixture, 0, sizeof(*fixture));
	if (mkdtemp(template) == NULL ||
	    snprintf(fixture->root, sizeof(fixture->root), "%s", template) >=
		    (int) sizeof(fixture->root) ||
	    make_path(fixture->sysfs, sizeof(fixture->sysfs),
		      fixture->root, "sys") < 0 ||
	    make_path(fixture->dev, sizeof(fixture->dev),
		      fixture->root, "dev") < 0 ||
	    make_path(fixture->proc, sizeof(fixture->proc),
		      fixture->root, "proc") < 0 ||
	    make_path(fixture->run_dir, sizeof(fixture->run_dir),
		      fixture->root, "run/orbi-bt-setup") < 0 ||
	    make_path(fixture->pending, sizeof(fixture->pending),
		      fixture->run_dir, "ana-freq.pending") < 0 ||
	    make_path(fixture->lock, sizeof(fixture->lock),
		      fixture->root, "run/orbi-bt-setup.lock") < 0 ||
	    make_path(fixture->hciextn, sizeof(fixture->hciextn),
		      fixture->root, "hciextn") < 0 ||
	    make_path(fixture->node, sizeof(fixture->node), fixture->sysfs,
		      "firmware/devicetree/base/soc/serial@78b0000") < 0 ||
	    make_path(fixture->platform, sizeof(fixture->platform),
		      fixture->sysfs,
		      "devices/platform/soc/78b0000.serial") < 0)
		return -1;
	if (mkdir_p(fixture->dev) < 0 || mkdir_p(fixture->proc) < 0 ||
	    mkdir_p(fixture->node) < 0 || mkdir_p(fixture->platform) < 0 ||
	    make_path(path, sizeof(path), fixture->root, "run") < 0 ||
	    mkdir_p(path) < 0)
		return -1;
	if (make_path(path, sizeof(path), fixture->sysfs,
		      "firmware/devicetree/base/compatible") < 0 ||
	    snprintf(data, sizeof(data), "%s%cqcom,ipq4019%c",
		     board, '\0', '\0') < 0 ||
	    write_data(path, data, strlen(board) + 1 +
		       strlen("qcom,ipq4019") + 1) < 0 ||
	    make_path(path, sizeof(path), fixture->node, "compatible") < 0 ||
	    write_data(path, "qcom,msm-uartdm-v1.4\0qcom,msm-uartdm\0",
		       sizeof("qcom,msm-uartdm-v1.4\0qcom,msm-uartdm\0") - 1) < 0 ||
	    make_path(path, sizeof(path), fixture->node, "status") < 0 ||
	    write_data(path, "okay\0", 5) < 0 ||
	    make_path(path, sizeof(path), fixture->node, "reg") < 0 ||
	    write_data(path, registers, sizeof(registers)) < 0)
		return -1;
	if (make_path(driver, sizeof(driver), fixture->sysfs,
		      "bus/platform/drivers/msm_serial") < 0 ||
	    mkdir_p(driver) < 0 ||
	    make_path(path, sizeof(path), fixture->platform, "of_node") < 0 ||
	    make_link(fixture->node, path) < 0 ||
	    make_path(path, sizeof(path), fixture->platform, "driver") < 0 ||
	    make_link(driver, path) < 0 ||
	    make_path(driver, sizeof(driver), fixture->sysfs,
		      "bus/platform") < 0 ||
	    mkdir_p(driver) < 0 ||
	    make_path(path, sizeof(path), fixture->platform, "subsystem") < 0 ||
	    make_link(driver, path) < 0)
		return -1;
	if (nested) {
		if (make_path(ctrl, sizeof(ctrl), fixture->platform,
			      "78b0000.serial:0") < 0 ||
		    make_path(fixture->port, sizeof(fixture->port), ctrl,
			      "78b0000.serial:0.0") < 0 ||
		    mkdir_p(fixture->port) < 0 ||
		    make_path(driver, sizeof(driver), fixture->sysfs,
			      "bus/serial-base/drivers/ctrl") < 0 ||
		    mkdir_p(driver) < 0 ||
		    make_path(path, sizeof(path), ctrl, "of_node") < 0 ||
		    make_link(fixture->node, path) < 0 ||
		    make_path(path, sizeof(path), ctrl, "driver") < 0 ||
		    make_link(driver, path) < 0 ||
		    make_path(driver, sizeof(driver), fixture->sysfs,
			      "bus/serial-base") < 0 ||
		    mkdir_p(driver) < 0 ||
		    make_path(path, sizeof(path), ctrl, "subsystem") < 0 ||
		    make_link(driver, path) < 0 ||
		    make_path(driver, sizeof(driver), fixture->sysfs,
			      "bus/serial-base/drivers/port") < 0 ||
		    mkdir_p(driver) < 0 ||
		    make_path(path, sizeof(path), fixture->port, "of_node") < 0 ||
		    make_link(fixture->node, path) < 0 ||
		    make_path(path, sizeof(path), fixture->port, "driver") < 0 ||
		    make_link(driver, path) < 0 ||
		    make_path(driver, sizeof(driver), fixture->sysfs,
			      "bus/serial-base") < 0 ||
		    make_path(path, sizeof(path), fixture->port, "subsystem") < 0 ||
		    make_link(driver, path) < 0)
			return -1;
	} else if (snprintf(fixture->port, sizeof(fixture->port), "%s",
			    fixture->platform) >= (int) sizeof(fixture->port)) {
		return -1;
	}
	if (make_path(fixture->tty, sizeof(fixture->tty), fixture->port,
		      "tty/ttyMSM7") < 0 ||
	    mkdir_p(fixture->tty) < 0 ||
	    make_path(path, sizeof(path), fixture->tty, "dev") < 0 ||
	    write_text(path, "4:64\n") < 0 ||
	    make_path(path, sizeof(path), fixture->tty, "device") < 0 ||
	    make_link(fixture->port, path) < 0 ||
	    make_path(path, sizeof(path), fixture->sysfs,
		      "class/tty/ttyMSM7") < 0 ||
	    make_link(fixture->tty, path) < 0 ||
	    make_path(path, sizeof(path), fixture->proc, "consoles") < 0 ||
	    write_text(path, "") < 0 ||
	    write_text(fixture->hciextn, "Y\n") < 0)
		return -1;
	fixture->paths.sysfs = fixture->sysfs;
	fixture->paths.dev = fixture->dev;
	fixture->paths.proc = fixture->proc;
	fixture->paths.run_dir = fixture->run_dir;
	fixture->paths.pending = fixture->pending;
	fixture->paths.lock = fixture->lock;
	fixture->paths.hciextn = fixture->hciextn;
	return 0;
}

static void fixture_destroy(struct fixture *fixture)
{
	CHECK(remove_tree(fixture->root) == 0);
}

static int fixture_add_hci(struct fixture *fixture, const char *name,
			   bool target)
{
	char path[PATH_MAX];
	char controller[PATH_MAX];

	if (target) {
		if (make_path(controller, sizeof(controller), fixture->tty,
			      "bluetooth") < 0 ||
		    make_path(controller, sizeof(controller), controller, name) < 0)
			return -1;
	} else if (make_path(controller, sizeof(controller), fixture->sysfs,
			     "devices/usb/bluetooth") < 0 ||
		   make_path(controller, sizeof(controller), controller, name) < 0) {
		return -1;
	}
	if (mkdir_p(controller) < 0 ||
	    make_path(path, sizeof(path), fixture->sysfs,
		      "class/bluetooth") < 0 ||
	    mkdir_p(path) < 0 ||
	    make_path(path, sizeof(path), path, name) < 0 ||
	    make_link(controller, path) < 0)
		return -1;
	return 0;
}

static void fixture_replace_hci(struct fixture *fixture,
				const char *old_name, const char *new_name)
{
	char path[PATH_MAX];

	CHECK(make_path(path, sizeof(path), fixture->sysfs,
			"class/bluetooth") == 0);
	CHECK(make_path(path, sizeof(path), path, old_name) == 0);
	CHECK(unlink(path) == 0);
	CHECK(make_path(path, sizeof(path), fixture->tty, "bluetooth") == 0);
	CHECK(make_path(path, sizeof(path), path, old_name) == 0);
	CHECK(remove_tree(path) == 0);
	CHECK(fixture_add_hci(fixture, new_name, true) == 0);
}

static void test_discovery_hierarchies(void)
{
	const char *boards[] = {"netgear,rbr50", "netgear,rbs50"};

	for (size_t board = 0; board < 2; board++) {
		for (unsigned int nested = 0; nested < 2; nested++) {
			struct fixture fixture;
			struct uart_identity uart;
			struct orbi_error error = {{0}};

			CHECK(fixture_init(&fixture, boards[board], nested != 0) == 0);
			if (orbi_discover_uart(&fixture.paths, &uart, &error) < 0) {
				fprintf(stderr, "discover %s nested=%u: %s\n",
					boards[board], nested, error.message);
				CHECK(false);
			}
			CHECK(strcmp(uart.name, "ttyMSM7") == 0);
			CHECK(strcmp(uart.platform_path, fixture.platform) == 0);
			CHECK(uart.major_number == 4 && uart.minor_number == 64);
			fixture_destroy(&fixture);
		}
	}
}

static void test_rejects_other_board(void)
{
	struct fixture fixture;
	struct uart_identity uart;
	struct orbi_error error = {{0}};

	CHECK(fixture_init(&fixture, "netgear,rbr50v2", true) == 0);
	CHECK(orbi_discover_uart(&fixture.paths, &uart, &error) < 0);
	fixture_destroy(&fixture);
}

static void test_rejects_wrong_ctrl_driver(void)
{
	struct fixture fixture;
	struct uart_identity uart;
	struct orbi_error error = {{0}};
	char ctrl[PATH_MAX];
	char path[PATH_MAX];
	char wrong[PATH_MAX];

	CHECK(fixture_init(&fixture, "netgear,rbs50", true) == 0);
	CHECK(snprintf(ctrl, sizeof(ctrl), "%s/78b0000.serial:0",
		       fixture.platform) < (int) sizeof(ctrl));
	CHECK(make_path(path, sizeof(path), ctrl, "driver") == 0);
	CHECK(make_path(wrong, sizeof(wrong), fixture.sysfs,
			"bus/serial-base/drivers/port") == 0);
	CHECK(replace_link(wrong, path) == 0);
	CHECK(orbi_discover_uart(&fixture.paths, &uart, &error) < 0);
	fixture_destroy(&fixture);
}

static void test_hci_selection(void)
{
	struct fixture fixture;
	struct controller_session session;
	struct orbi_error error = {{0}};

	CHECK(fixture_init(&fixture, "netgear,rbr50", true) == 0);
	CHECK(fixture_add_hci(&fixture, "hci8", false) == 0);
	CHECK(fixture_add_hci(&fixture, "hci4", true) == 0);
	CHECK(orbi_attached_session(&fixture.paths, &session, &error) == 0);
	CHECK(strcmp(session.hci.name, "hci4") == 0);
	CHECK(fixture_add_hci(&fixture, "hci5", true) == 0);
	CHECK(orbi_attached_session(&fixture.paths, &session, &error) < 0);
	fixture_destroy(&fixture);
}

static void test_device_symlink_rejected(void)
{
	struct fixture fixture;
	struct uart_identity uart;
	struct orbi_error error = {{0}};
	char path[PATH_MAX];

	CHECK(fixture_init(&fixture, "netgear,rbs50", true) == 0);
	CHECK(make_path(path, sizeof(path), fixture.dev, "ttyMSM7") == 0);
	CHECK(make_link("/dev/null", path) == 0);
	CHECK(orbi_available_uart(&fixture.paths, &uart, &error) < 0);
	fixture_destroy(&fixture);
}

static uint16_t get_le16_test(const uint8_t *data)
{
	return (uint16_t) data[0] | ((uint16_t) data[1] << 8);
}

static void put_le16_test(uint8_t *data, uint16_t value)
{
	data[0] = (uint8_t) (value & 0xffU);
	data[1] = (uint8_t) (value >> 8);
}

struct fake_ops {
	const char *pending;
	unsigned int power_up;
	unsigned int power_down;
	unsigned int confirms;
	unsigned int syncs;
	unsigned int gets;
	unsigned int sets;
	unsigned int warms;
	const char *failure;
	int mutate_word;
	bool malformed_event;
};

static int fake_power(const char *hci, bool up, void *opaque,
		      struct orbi_error *error)
{
	struct fake_ops *fake = opaque;

	(void) hci;
	if (up)
		fake->power_up++;
	else
		fake->power_down++;
	if ((up && fake->failure != NULL &&
	     strcmp(fake->failure, "up") == 0) ||
	    (!up && fake->failure != NULL &&
	     strcmp(fake->failure, "down") == 0)) {
		orbi_error_set(error, "injected power failure");
		return -1;
	}
	return 0;
}

static int fake_confirm(const char *hci, void *opaque,
			struct orbi_error *error)
{
	struct fake_ops *fake = opaque;

	(void) hci;
	fake->confirms++;
	if (fake->failure != NULL && strcmp(fake->failure, "confirm") == 0) {
		orbi_error_set(error, "injected confirm failure");
		return -1;
	}
	return 0;
}

static int fake_csr(const char *hci, const uint8_t *pdu, size_t pdu_length,
		    uint8_t *event, size_t *event_length, bool *dispatched,
		    unsigned int timeout_ms, void *opaque,
		    struct orbi_error *error)
{
	struct fake_ops *fake = opaque;
	uint16_t kind;
	uint16_t sequence;
	uint16_t variable;
	uint16_t value = 26000;
	uint16_t words[9] = {1, 9, 0, 0x7003, 0, 0x01fe, 1, 8, 0};

	(void) hci;
	(void) timeout_ms;
	if (pdu_length != 19) {
		orbi_error_set(error, "unexpected test PDU length");
		return -1;
	}
	kind = get_le16_test(pdu + 1);
	sequence = get_le16_test(pdu + 5);
	variable = get_le16_test(pdu + 7);
	*dispatched = true;
	if (fake->failure != NULL &&
	    strcmp(fake->failure, "undispatched") == 0) {
		*dispatched = false;
		orbi_error_set(error, "injected undispatched command");
		return -1;
	}
	if (variable == 0x4002) {
		fake->warms++;
		if (fake->failure != NULL &&
		    strcmp(fake->failure, "warm-undispatched") == 0) {
			*dispatched = false;
			orbi_error_set(error, "injected undispatched warm reset");
			return -1;
		}
		return -ETIMEDOUT;
	}
	if (kind == 2) {
		struct stat status;

		fake->sets++;
		if (stat(fake->pending, &status) < 0) {
			orbi_error_set(error, "SET ran before pending marker");
			return -1;
		}
		if (fake->failure != NULL && strcmp(fake->failure, "set") == 0) {
			orbi_error_set(error, "injected SET failure");
			return -1;
		}
	} else {
		fake->gets++;
		if (fake->failure != NULL &&
		    strcmp(fake->failure, "pre-get") == 0 && fake->gets == 1) {
			orbi_error_set(error, "injected GET failure");
			return -1;
		}
		if (fake->failure != NULL &&
		    strcmp(fake->failure, "post-get") == 0)
			value = 25000;
	}
	if (*event_length < 19) {
		orbi_error_set(error, "test event buffer too small");
		return -1;
	}
	words[2] = sequence;
	words[8] = kind == 2 ? 26000 : value;
	event[0] = 0xc2;
	for (size_t index = 0; index < 9; index++)
		put_le16_test(event + 1 + index * 2, words[index]);
	if (fake->mutate_word >= 0 && fake->mutate_word < 9)
		event[1 + (size_t) fake->mutate_word * 2] ^= 1;
	if (fake->malformed_event) {
		*event_length = 18;
		return 0;
	}
	*event_length = 19;
	return 0;
}

static void fake_sync(void *opaque)
{
	struct fake_ops *fake = opaque;

	fake->syncs++;
}

static uint16_t fake_sequence(void *opaque)
{
	(void) opaque;
	return 0x1000;
}

static struct setup_ops make_fake_setup(struct fake_ops *fake)
{
	const struct setup_ops ops = {
		.set_hci_power = fake_power,
		.confirm_hci_down = fake_confirm,
		.csr_command = fake_csr,
		.sync_storage = fake_sync,
		.next_sequence = fake_sequence,
		.opaque = fake,
	};

	return ops;
}

static void init_fake(struct fake_ops *fake, const char *pending)
{
	memset(fake, 0, sizeof(*fake));
	fake->pending = pending;
	fake->mutate_word = -1;
}

static void test_prepare_verify_transaction(void)
{
	struct fixture fixture;
	struct fake_ops fake;
	struct setup_ops ops;
	struct orbi_error error = {{0}};
	struct stat status;

	CHECK(fixture_init(&fixture, "netgear,rbs50", true) == 0);
	CHECK(fixture_add_hci(&fixture, "hci4", true) == 0);
	init_fake(&fake, fixture.pending);
	ops = make_fake_setup(&fake);
	if (orbi_prepare(&fixture.paths, &ops, &error) < 0) {
		fprintf(stderr, "prepare transaction: %s\n", error.message);
		CHECK(false);
	}
	CHECK(stat(fixture.pending, &status) == 0);
	CHECK(fake.power_up == 1 && fake.gets == 2 && fake.sets == 1 &&
	      fake.warms == 1 && fake.syncs == 1);
	fixture_replace_hci(&fixture, "hci4", "hci5");
	CHECK(orbi_verify(&fixture.paths, &ops, &error) == 0);
	CHECK(lstat(fixture.pending, &status) < 0 && errno == ENOENT);
	CHECK(fake.power_up == 2 && fake.power_down == 1 &&
	      fake.confirms == 1 && fake.gets == 3);
	fixture_destroy(&fixture);
}

static void test_marker_boundaries(void)
{
	const char *failures[] = {"pre-get", "set"};

	for (size_t index = 0; index < 2; index++) {
		struct fixture fixture;
		struct fake_ops fake;
		struct setup_ops ops;
		struct orbi_error error = {{0}};
		struct stat status;

		CHECK(fixture_init(&fixture, "netgear,rbr50", true) == 0);
		CHECK(fixture_add_hci(&fixture, "hci4", true) == 0);
		init_fake(&fake, fixture.pending);
		fake.failure = failures[index];
		ops = make_fake_setup(&fake);
		CHECK(orbi_prepare(&fixture.paths, &ops, &error) < 0);
		if (strcmp(failures[index], "set") == 0)
			CHECK(stat(fixture.pending, &status) == 0);
		else
			CHECK(lstat(fixture.pending, &status) < 0 &&
			      errno == ENOENT);
		fixture_destroy(&fixture);
	}
}

static void test_verify_failure_keeps_marker(void)
{
	struct fixture fixture;
	struct fake_ops fake;
	struct setup_ops ops;
	struct orbi_error error = {{0}};
	struct stat status;

	CHECK(fixture_init(&fixture, "netgear,rbs50", true) == 0);
	CHECK(fixture_add_hci(&fixture, "hci4", true) == 0);
	init_fake(&fake, fixture.pending);
	ops = make_fake_setup(&fake);
	if (orbi_prepare(&fixture.paths, &ops, &error) < 0) {
		fprintf(stderr, "prepare verify-failure case: %s\n",
			error.message);
		CHECK(false);
	}
	fixture_replace_hci(&fixture, "hci4", "hci5");
	fake.failure = "post-get";
	CHECK(orbi_verify(&fixture.paths, &ops, &error) < 0);
	CHECK(stat(fixture.pending, &status) == 0);
	fixture_destroy(&fixture);
}

static void test_response_identity(void)
{
	uint8_t request[19] = {0};
	uint8_t response[19] = {0};

	request[0] = 0xc2;
	response[0] = 0xc2;
	put_le16_test(request + 5, 0x1234);
	put_le16_test(request + 7, 0x7003);
	put_le16_test(response + 5, 0x1234);
	put_le16_test(response + 7, 0x7003);
	CHECK(orbi_csr_header_matches(response, sizeof(response),
				      request, sizeof(request)));
	response[5] ^= 1;
	CHECK(!orbi_csr_header_matches(response, sizeof(response),
				       request, sizeof(request)));
}

static void test_production_vendor_packet_processing(void)
{
	uint8_t pdu[19] = {0};
	uint8_t packet[22] = {0};
	uint8_t event[32] = {0};
	struct orbi_error error = {{0}};
	size_t event_length;

	pdu[0] = 0xc2;
	put_le16_test(pdu + 5, 0x1234);
	put_le16_test(pdu + 7, 0x7003);
	packet[0] = 0x04;
	packet[1] = 0xff;
	packet[2] = 19;
	packet[3] = 0xc2;
	put_le16_test(packet + 8, 0x1234);
	put_le16_test(packet + 10, 0x7003);
	event_length = sizeof(event);
	CHECK(orbi_test_process_vendor_packet(packet, sizeof(packet),
					      pdu, sizeof(pdu),
					      event, &event_length,
					      &error) == 0);
	CHECK(event_length == 19);
	CHECK(memcmp(event, packet + 3, 19) == 0);
	event_length = 18;
	CHECK(orbi_test_process_vendor_packet(packet, sizeof(packet),
					      pdu, sizeof(pdu),
					      event, &event_length,
					      &error) < 0);
	packet[8] ^= 1;
	event_length = sizeof(event);
	CHECK(orbi_test_process_vendor_packet(packet, sizeof(packet),
					      pdu, sizeof(pdu),
					      event, &event_length,
					      &error) == 1);
	CHECK(event_length == 0);
}

static void test_csr_response_mutations(void)
{
	const int identity_words[] = {0, 1, 2, 3, 5, 6, 7};

	for (size_t index = 0;
	     index < sizeof(identity_words) / sizeof(identity_words[0]);
	     index++) {
		struct fake_ops fake;
		struct setup_ops ops;
		struct orbi_error error = {{0}};
		uint16_t status;
		uint16_t value;

		init_fake(&fake, "/nonexistent");
		fake.mutate_word = identity_words[index];
		ops = make_fake_setup(&fake);
		if (orbi_csr_read_ana("hci4", 0x1234, &status,
				      &value, &ops, &error) == 0) {
			fprintf(stderr, "accepted mutated CSR word %d\n",
				identity_words[index]);
			CHECK(false);
		}
	}
	{
		struct fake_ops fake;
		struct setup_ops ops;
		struct orbi_error error = {{0}};
		uint16_t status;
		uint16_t value;

		init_fake(&fake, "/nonexistent");
		fake.malformed_event = true;
		ops = make_fake_setup(&fake);
		CHECK(orbi_csr_read_ana("hci4", 0x1234, &status,
					&value, &ops, &error) < 0);
	}
	{
		struct fake_ops fake;
		struct setup_ops ops;
		struct orbi_error error = {{0}};
		uint16_t status;
		uint16_t value;

		init_fake(&fake, "/nonexistent");
		fake.failure = "undispatched";
		ops = make_fake_setup(&fake);
		if (orbi_csr_read_ana("hci4", 0x1234, &status,
				      &value, &ops, &error) == 0) {
			fprintf(stderr, "accepted undispatched CSR command\n");
			CHECK(false);
		}
	}
}

static void test_unrelated_tty_is_ignored(void)
{
	struct fixture fixture;
	struct uart_identity uart;
	struct orbi_error error = {{0}};
	char other_node[PATH_MAX];
	char other_device[PATH_MAX];
	char other_tty[PATH_MAX];
	char path[PATH_MAX];

	CHECK(fixture_init(&fixture, "netgear,rbs50", true) == 0);
	CHECK(make_path(other_node, sizeof(other_node), fixture.sysfs,
			"firmware/devicetree/base/soc/serial@other") == 0);
	CHECK(make_path(other_device, sizeof(other_device), fixture.sysfs,
			"devices/platform/soc/other.serial") == 0);
	CHECK(make_path(other_tty, sizeof(other_tty), other_device,
			"tty/ttyMSM8") == 0);
	CHECK(mkdir_p(other_node) == 0);
	CHECK(mkdir_p(other_tty) == 0);
	CHECK(make_path(path, sizeof(path), other_device, "of_node") == 0);
	CHECK(make_link(other_node, path) == 0);
	CHECK(make_path(path, sizeof(path), other_tty, "dev") == 0);
	CHECK(write_text(path, "4:65\n") == 0);
	CHECK(make_path(path, sizeof(path), other_tty, "device") == 0);
	CHECK(make_link(other_device, path) == 0);
	CHECK(make_path(path, sizeof(path), fixture.sysfs,
			"class/tty/ttyMSM8") == 0);
	CHECK(make_link(other_tty, path) == 0);
	CHECK(orbi_discover_uart(&fixture.paths, &uart, &error) == 0);
	CHECK(strcmp(uart.name, "ttyMSM7") == 0);
	fixture_destroy(&fixture);
}

static void test_device_identity_console_and_open_fd(void)
{
	struct fixture fixture;
	struct uart_identity uart;
	struct orbi_error error = {{0}};
	char device[PATH_MAX];
	char path[PATH_MAX];

	CHECK(fixture_init(&fixture, "netgear,rbr50", true) == 0);
	CHECK(make_path(device, sizeof(device), fixture.dev, "ttyMSM7") == 0);
	if (mknod(device, S_IFCHR | 0600, makedev(4, 64)) < 0 &&
	    errno == EPERM) {
		fixture_destroy(&fixture);
		return;
	}
	CHECK(errno == 0 || access(device, F_OK) == 0);
	CHECK(orbi_available_uart(&fixture.paths, &uart, &error) == 0);
	CHECK(make_path(path, sizeof(path), fixture.proc, "consoles") == 0);
	CHECK(write_text(path, "ttyMSM7 -W- (EC p a) 4:64\n") == 0);
	CHECK(orbi_available_uart(&fixture.paths, &uart, &error) < 0);
	CHECK(write_text(path, "") == 0);
	CHECK(make_path(path, sizeof(path), fixture.proc, "123/fd/4") == 0);
	CHECK(make_link(device, path) == 0);
	CHECK(orbi_available_uart(&fixture.paths, &uart, &error) < 0);
	fixture_destroy(&fixture);
}

static void test_fails_closed_core_fields(void)
{
	const char *fields[] = {"status", "reg", "compatible", "ambiguous"};

	for (size_t index = 0; index < 4; index++) {
		struct fixture fixture;
		struct uart_identity uart;
		struct orbi_error error = {{0}};
		char path[PATH_MAX];
		uint8_t zero_reg[8] = {0};

		CHECK(fixture_init(&fixture, "netgear,rbr50", true) == 0);
		if (strcmp(fields[index], "status") == 0) {
			CHECK(make_path(path, sizeof(path), fixture.node,
					"status") == 0);
			CHECK(write_data(path, "disabled\0", 9) == 0);
		} else if (strcmp(fields[index], "reg") == 0) {
			CHECK(make_path(path, sizeof(path), fixture.node,
					"reg") == 0);
			CHECK(write_data(path, zero_reg, sizeof(zero_reg)) == 0);
		} else if (strcmp(fields[index], "compatible") == 0) {
			CHECK(make_path(path, sizeof(path), fixture.node,
					"compatible") == 0);
			CHECK(write_data(path, "not-the-uart\0", 13) == 0);
		} else {
			CHECK(make_path(path, sizeof(path), fixture.sysfs,
					"class/tty/ttyMSM8") == 0);
			CHECK(make_link(fixture.tty, path) == 0);
		}
		CHECK(orbi_discover_uart(&fixture.paths, &uart, &error) < 0);
		fixture_destroy(&fixture);
	}
}

static void test_marker_strictness_and_identity(void)
{
	struct fixture fixture;
	struct pending_transaction pending;
	struct pending_transaction parsed;
	struct uart_identity uart;
	struct orbi_error error = {{0}};

	CHECK(fixture_init(&fixture, "netgear,rbs50", true) == 0);
	CHECK(orbi_discover_uart(&fixture.paths, &uart, &error) == 0);
	memset(&pending, 0, sizeof(pending));
	pending.uart = uart;
	pending.post_sequence = 7;
	CHECK(orbi_write_pending(&fixture.paths, &pending, &error) == 0);
	CHECK(orbi_read_pending(fixture.pending, &parsed, &error) == 0);
	CHECK(orbi_verify_pending_uart(&parsed, &uart, &error) == 0);
	parsed.uart.inode++;
	CHECK(orbi_verify_pending_uart(&parsed, &uart, &error) < 0);
	CHECK(chmod(fixture.pending, 0644) == 0);
	CHECK(orbi_read_pending(fixture.pending, &parsed, &error) < 0);
	CHECK(chmod(fixture.pending, 0600) == 0);
	CHECK(write_text(fixture.pending, "invalid\n") == 0);
	CHECK(orbi_read_pending(fixture.pending, &parsed, &error) < 0);
	fixture_destroy(&fixture);
}

static void test_path_containment(void)
{
	CHECK(orbi_path_beneath("/sys/devices/a", "/sys/devices"));
	CHECK(!orbi_path_beneath("/sys/device", "/sys/devices"));
	CHECK(!orbi_path_beneath("/sys/devices", "/sys/devices"));
	CHECK(!orbi_path_beneath("/sys/devicesx/a", "/sys/devices"));
	CHECK(!orbi_path_beneath("", "/sys/devices"));
}

static void test_lock_exclusion(void)
{
	struct fixture fixture;
	struct orbi_error error = {{0}};
	int first = -1;
	int second = -1;

	CHECK(fixture_init(&fixture, "netgear,rbr50", false) == 0);
	CHECK(orbi_lock_setup(fixture.lock, &first, &error) == 0);
	CHECK(orbi_lock_setup(fixture.lock, &second, &error) < 0);
	CHECK(close(first) == 0);
	CHECK(orbi_lock_setup(fixture.lock, &second, &error) == 0);
	CHECK(close(second) == 0);
	fixture_destroy(&fixture);
}

static void test_hciextn_gate(void)
{
	struct fixture fixture;
	struct orbi_error error = {{0}};

	CHECK(fixture_init(&fixture, "netgear,rbr50", false) == 0);
	CHECK(orbi_require_hciextn(fixture.hciextn, &error) == 0);
	CHECK(write_text(fixture.hciextn, "N\n") == 0);
	CHECK(orbi_require_hciextn(fixture.hciextn, &error) < 0);
	fixture_destroy(&fixture);
}

int main(void)
{
	test_discovery_hierarchies();
	test_rejects_other_board();
	test_rejects_wrong_ctrl_driver();
	test_unrelated_tty_is_ignored();
	test_fails_closed_core_fields();
	test_hci_selection();
	test_device_symlink_rejected();
	test_device_identity_console_and_open_fd();
	test_prepare_verify_transaction();
	test_marker_boundaries();
	test_verify_failure_keeps_marker();
	test_marker_strictness_and_identity();
	test_lock_exclusion();
	test_path_containment();
	test_response_identity();
	test_production_vendor_packet_processing();
	test_csr_response_mutations();
	test_hciextn_gate();
	if (tests_failed != 0) {
		fprintf(stderr, "%u of %u checks failed\n",
			tests_failed, tests_run);
		return 1;
	}
	printf("%u checks passed\n", tests_run);
	return 0;
}

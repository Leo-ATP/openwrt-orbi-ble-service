/* SPDX-License-Identifier: GPL-2.0-only */

#ifndef ORBI_BT_SETUP_H
#define ORBI_BT_SETUP_H

#include <limits.h>
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include <sys/types.h>

#define ORBI_BT_VERSION "0.3.0"
#define ORBI_TTY_NAME_MAX 32
#define ORBI_HCI_NAME_MAX 32
#define ORBI_ERROR_MAX 512
#define ORBI_CSR_PDU_SIZE 19
#define ORBI_CSR_EVENT_MAX 255

struct orbi_error {
	char message[ORBI_ERROR_MAX];
};

struct orbi_paths {
	const char *sysfs;
	const char *dev;
	const char *proc;
	const char *run_dir;
	const char *pending;
	const char *lock;
	const char *hciextn;
};

struct uart_identity {
	char name[ORBI_TTY_NAME_MAX];
	char tty_path[PATH_MAX];
	char platform_path[PATH_MAX];
	dev_t stat_device;
	ino_t inode;
	unsigned int major_number;
	unsigned int minor_number;
};

struct hci_identity {
	char name[ORBI_HCI_NAME_MAX];
	dev_t stat_device;
	ino_t inode;
};

struct controller_session {
	struct uart_identity uart;
	struct hci_identity hci;
};

struct pending_transaction {
	struct uart_identity uart;
	uint16_t post_sequence;
};

struct setup_ops {
	int (*set_hci_power)(const char *hci, bool up, void *opaque,
			    struct orbi_error *error);
	int (*confirm_hci_down)(const char *hci, void *opaque,
			       struct orbi_error *error);
	int (*csr_command)(const char *hci, const uint8_t *pdu, size_t pdu_length,
			  uint8_t *event, size_t *event_length,
			  bool *dispatched, unsigned int timeout_ms,
			  void *opaque, struct orbi_error *error);
	void (*sync_storage)(void *opaque);
	uint16_t (*next_sequence)(void *opaque);
	void *opaque;
};

void orbi_error_set(struct orbi_error *error, const char *format, ...)
	__attribute__((format(printf, 2, 3)));

bool orbi_path_beneath(const char *path, const char *parent);
int orbi_discover_uart(const struct orbi_paths *paths,
		       struct uart_identity *uart, struct orbi_error *error);
int orbi_available_uart(const struct orbi_paths *paths,
			struct uart_identity *uart, struct orbi_error *error);
int orbi_find_hci(const struct orbi_paths *paths,
		  const struct uart_identity *uart, struct hci_identity *hci,
		  struct orbi_error *error);
int orbi_attached_session(const struct orbi_paths *paths,
			  struct controller_session *session,
			  struct orbi_error *error);
int orbi_verify_session(const struct orbi_paths *paths,
			const struct controller_session *session,
			struct orbi_error *error);

int orbi_require_hciextn(const char *path, struct orbi_error *error);
int orbi_csr_command_raw(const char *hci, const uint8_t *pdu,
			 size_t pdu_length, uint8_t *event,
			 size_t *event_length, bool *dispatched,
			 unsigned int timeout_ms, void *opaque,
			 struct orbi_error *error);
bool orbi_csr_header_matches(const uint8_t *event, size_t event_length,
			     const uint8_t *pdu, size_t pdu_length);
int orbi_csr_read_ana(const char *hci, uint16_t sequence,
		      uint16_t *status, uint16_t *value,
		      const struct setup_ops *ops, struct orbi_error *error);
int orbi_csr_set_ana(const char *hci, uint16_t sequence,
		     const struct setup_ops *ops, struct orbi_error *error);
int orbi_csr_warm_reset(const char *hci, uint16_t sequence,
			const struct setup_ops *ops, struct orbi_error *error);

#ifdef ORBI_BT_TESTING
int orbi_test_process_vendor_packet(const uint8_t *packet,
				    size_t packet_length,
				    const uint8_t *pdu,
				    size_t pdu_length,
				    uint8_t *event,
				    size_t *event_length,
				    struct orbi_error *error);
#endif

int orbi_lock_setup(const char *path, int *fd, struct orbi_error *error);
int orbi_require_no_pending(const char *path, struct orbi_error *error);
int orbi_write_pending(const struct orbi_paths *paths,
		       const struct pending_transaction *pending,
		       struct orbi_error *error);
int orbi_read_pending(const char *path,
		      struct pending_transaction *pending,
		      struct orbi_error *error);
int orbi_clear_pending(const char *path, struct orbi_error *error);
int orbi_verify_pending_uart(const struct pending_transaction *pending,
			     const struct uart_identity *uart,
			     struct orbi_error *error);

int orbi_prepare(const struct orbi_paths *paths, const struct setup_ops *ops,
		 struct orbi_error *error);
int orbi_verify(const struct orbi_paths *paths, const struct setup_ops *ops,
		struct orbi_error *error);

int orbi_default_set_hci_power(const char *hci, bool up, void *opaque,
			       struct orbi_error *error);
int orbi_default_confirm_hci_down(const char *hci, void *opaque,
				  struct orbi_error *error);
void orbi_default_sync_storage(void *opaque);
uint16_t orbi_default_next_sequence(void *opaque);

#endif

/* SPDX-License-Identifier: GPL-2.0-only */

#include "orbi_bt_setup.h"

#include <errno.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>

static const struct orbi_paths default_paths = {
	.sysfs = "/sys",
	.dev = "/dev",
	.proc = "/proc",
	.run_dir = "/var/run/orbi-bt-setup",
	.pending = "/var/run/orbi-bt-setup/ana-freq.pending",
	.lock = "/var/run/orbi-bt-setup.lock",
	.hciextn = "/sys/module/hci_uart/parameters/hciextn",
};

static const struct setup_ops default_ops = {
	.set_hci_power = orbi_default_set_hci_power,
	.confirm_hci_down = orbi_default_confirm_hci_down,
	.csr_command = orbi_csr_command_raw,
	.sync_storage = orbi_default_sync_storage,
	.next_sequence = orbi_default_next_sequence,
	.opaque = NULL,
};

static void usage(FILE *stream)
{
	fprintf(stream,
		"usage: orbi-bt-setup tty | hci | prepare | verify\n");
}

int main(int argc, char **argv)
{
	struct orbi_error error = {{0}};
	struct controller_session session;
	struct uart_identity uart;
	int lock_fd = -1;
	int result = 1;

	if (argc == 2 &&
	    (strcmp(argv[1], "--version") == 0 ||
	     strcmp(argv[1], "-V") == 0)) {
		printf("orbi-bt-setup %s\n", ORBI_BT_VERSION);
		return 0;
	}
	if (argc != 2) {
		usage(stderr);
		return 2;
	}
	if (orbi_lock_setup(default_paths.lock, &lock_fd, &error) < 0)
		goto out;
	if (strcmp(argv[1], "tty") == 0) {
		if (orbi_require_no_pending(default_paths.pending, &error) < 0 ||
		    orbi_available_uart(&default_paths, &uart, &error) < 0)
			goto out;
		printf("%s/%s\n", default_paths.dev, uart.name);
		result = 0;
	} else if (strcmp(argv[1], "hci") == 0) {
		if (orbi_attached_session(&default_paths, &session, &error) < 0)
			goto out;
		printf("%s\n", session.hci.name);
		result = 0;
	} else if (strcmp(argv[1], "prepare") == 0) {
		if (orbi_prepare(&default_paths, &default_ops, &error) < 0)
			goto out;
		result = 0;
	} else if (strcmp(argv[1], "verify") == 0) {
		if (orbi_verify(&default_paths, &default_ops, &error) < 0)
			goto out;
		result = 0;
	} else {
		usage(stderr);
		return 2;
	}

out:
	if (lock_fd >= 0 && close(lock_fd) < 0 && result == 0) {
		orbi_error_set(&error, "close setup lock: %s", strerror(errno));
		result = 1;
	}
	if (result != 0 && error.message[0] != '\0')
		fprintf(stderr, "orbi-bt-setup: %s\n", error.message);
	return result;
}

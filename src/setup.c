/* SPDX-License-Identifier: GPL-2.0-only */

#include "orbi_bt_setup.h"

#include <errno.h>
#include <fcntl.h>
#include <inttypes.h>
#include <poll.h>
#include <signal.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/file.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

#define COMMAND_TIMEOUT_MS 5000
#define CHILD_OUTPUT_MAX 4096
#define MARKER_VERSION "ORBI_BT_PENDING_V1"

struct guarded_context {
	const struct orbi_paths *paths;
	const struct controller_session *session;
	const struct setup_ops *base;
};

void orbi_error_set(struct orbi_error *error, const char *format, ...)
{
	va_list arguments;

	if (error == NULL)
		return;
	va_start(arguments, format);
	(void) vsnprintf(error->message, sizeof(error->message),
			 format, arguments);
	va_end(arguments);
}

int orbi_lock_setup(const char *path, int *fd, struct orbi_error *error)
{
	int file = open(path, O_CREAT | O_RDWR | O_CLOEXEC | O_NOFOLLOW, 0600);

	if (file < 0) {
		orbi_error_set(error, "open setup lock: %s", strerror(errno));
		return -1;
	}
	if (flock(file, LOCK_EX | LOCK_NB) < 0) {
		orbi_error_set(error, "another setup command is running: %s",
			       strerror(errno));
		close(file);
		return -1;
	}
	*fd = file;
	return 0;
}

static int secure_directory(const char *path, struct orbi_error *error)
{
	struct stat status;

	if (lstat(path, &status) < 0) {
		if (errno != ENOENT) {
			orbi_error_set(error, "inspect runtime directory: %s",
				       strerror(errno));
			return -1;
		}
		if (mkdir(path, 0700) < 0) {
			orbi_error_set(error, "create runtime directory: %s",
				       strerror(errno));
			return -1;
		}
	} else if (!S_ISDIR(status.st_mode) || S_ISLNK(status.st_mode)) {
		orbi_error_set(error,
			       "runtime path is not a direct directory");
		return -1;
	}
	if (chmod(path, 0700) < 0) {
		orbi_error_set(error, "protect runtime directory: %s",
			       strerror(errno));
		return -1;
	}
	return 0;
}

int orbi_require_no_pending(const char *path, struct orbi_error *error)
{
	struct stat status;

	if (lstat(path, &status) < 0) {
		if (errno == ENOENT)
			return 0;
		orbi_error_set(error, "inspect pending transaction: %s",
			       strerror(errno));
		return -1;
	}
	orbi_error_set(error,
		       "a previous ANA_FREQ transaction did not finish; reboot before retrying");
	return -1;
}

static int write_all(int fd, const char *data, size_t length,
		     struct orbi_error *error)
{
	size_t offset = 0;

	while (offset < length) {
		ssize_t written = write(fd, data + offset, length - offset);

		if (written < 0 && errno == EINTR)
			continue;
		if (written < 0) {
			orbi_error_set(error, "write pending transaction: %s",
				       strerror(errno));
			return -1;
		}
		if (written == 0) {
			orbi_error_set(error,
				       "pending transaction write made no progress");
			return -1;
		}
		offset += (size_t) written;
	}
	return 0;
}

int orbi_write_pending(const struct orbi_paths *paths,
		       const struct pending_transaction *pending,
		       struct orbi_error *error)
{
	char data[PATH_MAX * 2 + 512];
	int length;
	int fd;
	int result = -1;

	if (secure_directory(paths->run_dir, error) < 0)
		return -1;
	length = snprintf(data, sizeof(data),
			  MARKER_VERSION "\n"
			  "tty_name=%s\n"
			  "tty_path=%s\n"
			  "platform_path=%s\n"
			  "stat_device=%" PRIuMAX "\n"
			  "inode=%" PRIuMAX "\n"
			  "major=%u\n"
			  "minor=%u\n"
			  "post_sequence=%u\n",
			  pending->uart.name,
			  pending->uart.tty_path,
			  pending->uart.platform_path,
			  (uintmax_t) pending->uart.stat_device,
			  (uintmax_t) pending->uart.inode,
			  pending->uart.major_number,
			  pending->uart.minor_number,
			  pending->post_sequence);
	if (length < 0 || (size_t) length >= sizeof(data)) {
		orbi_error_set(error, "pending transaction is too large");
		return -1;
	}
	fd = open(paths->pending,
		  O_CREAT | O_EXCL | O_WRONLY | O_CLOEXEC | O_NOFOLLOW, 0600);
	if (fd < 0) {
		orbi_error_set(error, "create pending transaction: %s",
			       strerror(errno));
		return -1;
	}
	if (write_all(fd, data, (size_t) length, error) < 0)
		goto out;
	if (fsync(fd) < 0) {
		orbi_error_set(error, "sync pending transaction: %s",
			       strerror(errno));
		goto out;
	}
	result = 0;

out:
	if (close(fd) < 0 && result == 0) {
		orbi_error_set(error, "close pending transaction: %s",
			       strerror(errno));
		result = -1;
	}
	return result;
}

static int parse_text_field(const char *line, const char *prefix,
			    char *output, size_t capacity,
			    struct orbi_error *error)
{
	size_t prefix_length = strlen(prefix);
	size_t value_length;

	if (strncmp(line, prefix, prefix_length) != 0) {
		orbi_error_set(error, "pending transaction field mismatch");
		return -1;
	}
	value_length = strlen(line + prefix_length);
	if (value_length == 0 || value_length >= capacity) {
		orbi_error_set(error, "invalid pending transaction text field");
		return -1;
	}
	memcpy(output, line + prefix_length, value_length + 1);
	return 0;
}

static int parse_uintmax_field(const char *line, const char *prefix,
			       uintmax_t *value, struct orbi_error *error)
{
	size_t prefix_length = strlen(prefix);
	char *end = NULL;
	uintmax_t parsed;

	if (strncmp(line, prefix, prefix_length) != 0) {
		orbi_error_set(error, "pending transaction field mismatch");
		return -1;
	}
	errno = 0;
	parsed = strtoumax(line + prefix_length, &end, 10);
	if (errno != 0 || end == line + prefix_length || *end != '\0') {
		orbi_error_set(error, "invalid pending transaction number");
		return -1;
	}
	*value = parsed;
	return 0;
}

int orbi_read_pending(const char *path,
		      struct pending_transaction *pending,
		      struct orbi_error *error)
{
	char data[PATH_MAX * 2 + 512];
	char *lines[10];
	char *save = NULL;
	char *line;
	struct stat status;
	uintmax_t value;
	ssize_t length;
	size_t count = 0;
	int fd;

	fd = open(path, O_RDONLY | O_CLOEXEC | O_NOFOLLOW);
	if (fd < 0) {
		orbi_error_set(error, "open pending transaction: %s",
			       strerror(errno));
		return -1;
	}
	if (fstat(fd, &status) < 0 || !S_ISREG(status.st_mode)) {
		orbi_error_set(error, "pending transaction is not a regular file");
		close(fd);
		return -1;
	}
	if ((status.st_mode & 0777) != 0600 || status.st_uid != geteuid()) {
		orbi_error_set(error,
			       "pending transaction has unsafe ownership or mode");
		close(fd);
		return -1;
	}
	do {
		length = read(fd, data, sizeof(data) - 1);
	} while (length < 0 && errno == EINTR);
	if (length < 0) {
		orbi_error_set(error, "read pending transaction: %s",
			       strerror(errno));
		close(fd);
		return -1;
	}
	if ((size_t) length == sizeof(data) - 1) {
		char extra;
		ssize_t more;

		do {
			more = read(fd, &extra, 1);
		} while (more < 0 && errno == EINTR);
		if (more != 0) {
			orbi_error_set(error, "pending transaction is too large");
			close(fd);
			return -1;
		}
	}
	if (close(fd) < 0) {
		orbi_error_set(error, "close pending transaction: %s",
			       strerror(errno));
		return -1;
	}
	data[length] = '\0';
	for (line = strtok_r(data, "\n", &save);
	     line != NULL && count < 10;
	     line = strtok_r(NULL, "\n", &save))
		lines[count++] = line;
	if (line != NULL || count != 9 || strcmp(lines[0], MARKER_VERSION) != 0) {
		orbi_error_set(error, "invalid pending transaction format");
		return -1;
	}
	memset(pending, 0, sizeof(*pending));
	if (parse_text_field(lines[1], "tty_name=", pending->uart.name,
			     sizeof(pending->uart.name), error) < 0 ||
	    parse_text_field(lines[2], "tty_path=", pending->uart.tty_path,
			     sizeof(pending->uart.tty_path), error) < 0 ||
	    parse_text_field(lines[3], "platform_path=",
			     pending->uart.platform_path,
			     sizeof(pending->uart.platform_path), error) < 0 ||
	    parse_uintmax_field(lines[4], "stat_device=", &value, error) < 0)
		return -1;
	pending->uart.stat_device = (dev_t) value;
	if ((uintmax_t) pending->uart.stat_device != value) {
		orbi_error_set(error, "pending stat_device is out of range");
		return -1;
	}
	if (parse_uintmax_field(lines[5], "inode=", &value, error) < 0)
		return -1;
	pending->uart.inode = (ino_t) value;
	if ((uintmax_t) pending->uart.inode != value) {
		orbi_error_set(error, "pending inode is out of range");
		return -1;
	}
	if (parse_uintmax_field(lines[6], "major=", &value, error) < 0)
		return -1;
	if (value > UINT_MAX) {
		orbi_error_set(error, "pending major is out of range");
		return -1;
	}
	pending->uart.major_number = (unsigned int) value;
	if (parse_uintmax_field(lines[7], "minor=", &value, error) < 0)
		return -1;
	if (value > UINT_MAX) {
		orbi_error_set(error, "pending minor is out of range");
		return -1;
	}
	pending->uart.minor_number = (unsigned int) value;
	if (parse_uintmax_field(lines[8], "post_sequence=", &value, error) < 0)
		return -1;
	if (value > UINT16_MAX) {
		orbi_error_set(error, "pending sequence is out of range");
		return -1;
	}
	pending->post_sequence = (uint16_t) value;
	return 0;
}

int orbi_clear_pending(const char *path, struct orbi_error *error)
{
	if (unlink(path) < 0 && errno != ENOENT) {
		orbi_error_set(error, "clear pending transaction: %s",
			       strerror(errno));
		return -1;
	}
	return 0;
}

int orbi_verify_pending_uart(const struct pending_transaction *pending,
			     const struct uart_identity *uart,
			     struct orbi_error *error)
{
	if (strcmp(pending->uart.name, uart->name) != 0 ||
	    strcmp(pending->uart.tty_path, uart->tty_path) != 0 ||
	    strcmp(pending->uart.platform_path, uart->platform_path) != 0 ||
	    pending->uart.stat_device != uart->stat_device ||
	    pending->uart.inode != uart->inode ||
	    pending->uart.major_number != uart->major_number ||
	    pending->uart.minor_number != uart->minor_number) {
		orbi_error_set(error,
			       "Bluetooth UART identity changed across warm reset");
		return -1;
	}
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

static int run_program(char *const arguments[], char *output, size_t capacity,
		       struct orbi_error *error)
{
	int pipe_fd[2] = {-1, -1};
	pid_t child = -1;
	size_t used = 0;
	int64_t deadline;
	int result = -1;
	int status = 0;
	bool exited = false;

	if (pipe2(pipe_fd, O_CLOEXEC | O_NONBLOCK) < 0) {
		orbi_error_set(error, "create command pipe: %s", strerror(errno));
		return -1;
	}
	child = fork();
	if (child < 0) {
		orbi_error_set(error, "fork command: %s", strerror(errno));
		goto out;
	}
	if (child == 0) {
		(void) close(pipe_fd[0]);
		if (dup2(pipe_fd[1], STDOUT_FILENO) < 0 ||
		    dup2(pipe_fd[1], STDERR_FILENO) < 0)
			_exit(126);
		(void) close(pipe_fd[1]);
		execv(arguments[0], arguments);
		_exit(127);
	}
	(void) close(pipe_fd[1]);
	pipe_fd[1] = -1;
	deadline = monotonic_milliseconds(error);
	if (deadline < 0)
		goto terminate;
	deadline += COMMAND_TIMEOUT_MS;
	while (!exited) {
		struct pollfd descriptor = {
			.fd = pipe_fd[0],
			.events = POLLIN,
		};
		int64_t now = monotonic_milliseconds(error);
		int timeout;
		int poll_result;
		pid_t wait_result;

		if (now < 0)
			goto terminate;
		if (now >= deadline) {
			orbi_error_set(error, "command timed out");
			goto terminate;
		}
		timeout = (int) (deadline - now);
		poll_result = poll(&descriptor, 1, timeout > 100 ? 100 : timeout);
		if (poll_result < 0 && errno != EINTR) {
			orbi_error_set(error, "poll command: %s", strerror(errno));
			goto terminate;
		}
		if (poll_result > 0 && (descriptor.revents & POLLIN) != 0) {
			char discard[256];
			char *target = discard;
			size_t available = sizeof(discard);
			ssize_t length;

			if (capacity > used + 1) {
				target = output + used;
				available = capacity - used - 1;
			}
			length = read(pipe_fd[0], target, available);

			if (length > 0 && target != discard)
				used += (size_t) length;
			else if (length < 0 && errno != EINTR && errno != EAGAIN) {
				orbi_error_set(error, "read command output: %s",
					       strerror(errno));
				goto terminate;
			}
		}
		wait_result = waitpid(child, &status, WNOHANG);
		if (wait_result < 0 && errno != EINTR) {
			orbi_error_set(error, "wait command: %s", strerror(errno));
			goto terminate;
		}
		exited = wait_result == child;
	}
	for (;;) {
		ssize_t length;

		if (capacity <= used + 1)
			break;
		length = read(pipe_fd[0], output + used, capacity - used - 1);
		if (length > 0) {
			used += (size_t) length;
			continue;
		}
		if (length < 0 && errno == EINTR)
			continue;
		break;
	}
	output[used] = '\0';
	if (!WIFEXITED(status) || WEXITSTATUS(status) != 0) {
		orbi_error_set(error, "command failed: %s", output);
		goto out;
	}
	result = 0;
	goto out;

terminate:
	if (child > 0) {
		(void) kill(child, SIGTERM);
		for (unsigned int attempt = 0; attempt < 20; attempt++) {
			pid_t waited = waitpid(child, &status, WNOHANG);

			if (waited == child) {
				exited = true;
				break;
			}
			if (waited < 0 && errno != EINTR)
				break;
			(void) usleep(100000);
		}
		if (!exited) {
			(void) kill(child, SIGKILL);
			while (waitpid(child, &status, 0) < 0 && errno == EINTR)
				;
		}
	}

out:
	if (pipe_fd[0] >= 0)
		close(pipe_fd[0]);
	if (pipe_fd[1] >= 0)
		close(pipe_fd[1]);
	return result;
}

int orbi_default_set_hci_power(const char *hci, bool up, void *opaque,
			       struct orbi_error *error)
{
	char executable[] = "/usr/bin/hciconfig";
	char action_up[] = "up";
	char action_down[] = "down";
	char hci_argument[ORBI_HCI_NAME_MAX];
	char *arguments[] = {
		executable,
		hci_argument,
		up ? action_up : action_down,
		NULL,
	};
	char output[CHILD_OUTPUT_MAX];

	(void) opaque;
	if (snprintf(hci_argument, sizeof(hci_argument), "%s", hci) >=
	    (int) sizeof(hci_argument)) {
		orbi_error_set(error, "HCI controller name is too long");
		return -1;
	}
	return run_program(arguments, output, sizeof(output), error);
}

int orbi_default_confirm_hci_down(const char *hci, void *opaque,
				  struct orbi_error *error)
{
	char executable[] = "/usr/bin/hciconfig";
	char hci_argument[ORBI_HCI_NAME_MAX];
	char *arguments[] = {
		executable,
		hci_argument,
		NULL,
	};
	char output[CHILD_OUTPUT_MAX];

	(void) opaque;
	if (snprintf(hci_argument, sizeof(hci_argument), "%s", hci) >=
	    (int) sizeof(hci_argument)) {
		orbi_error_set(error, "HCI controller name is too long");
		return -1;
	}
	if (run_program(arguments, output, sizeof(output), error) < 0)
		return -1;
	if (strstr(output, "DOWN") == NULL) {
		orbi_error_set(error, "%s did not report DOWN", hci);
		return -1;
	}
	return 0;
}

void orbi_default_sync_storage(void *opaque)
{
	(void) opaque;
	sync();
}

uint16_t orbi_default_next_sequence(void *opaque)
{
	struct timespec now;
	uint64_t value;

	(void) opaque;
	if (clock_gettime(CLOCK_MONOTONIC, &now) < 0)
		return 0;
	value = (uint64_t) now.tv_sec * 1000000000ULL +
		(uint64_t) now.tv_nsec;
	return (uint16_t) value;
}

static int guarded_power(const char *hci, bool up, void *opaque,
			 struct orbi_error *error)
{
	struct guarded_context *guard = opaque;

	if (orbi_verify_session(guard->paths, guard->session, error) < 0)
		return -1;
	return guard->base->set_hci_power(hci, up,
					 guard->base->opaque, error);
}

static int guarded_confirm_down(const char *hci, void *opaque,
				struct orbi_error *error)
{
	struct guarded_context *guard = opaque;

	if (orbi_verify_session(guard->paths, guard->session, error) < 0)
		return -1;
	return guard->base->confirm_hci_down(hci,
					    guard->base->opaque, error);
}

static int guarded_csr(const char *hci, const uint8_t *pdu,
		       size_t pdu_length, uint8_t *event,
		       size_t *event_length, bool *dispatched,
		       unsigned int timeout_ms, void *opaque,
		       struct orbi_error *error)
{
	struct guarded_context *guard = opaque;

	if (orbi_verify_session(guard->paths, guard->session, error) < 0)
		return -1;
	return guard->base->csr_command(hci, pdu, pdu_length,
					event, event_length, dispatched,
					timeout_ms, guard->base->opaque, error);
}

static void guarded_sync(void *opaque)
{
	struct guarded_context *guard = opaque;

	guard->base->sync_storage(guard->base->opaque);
}

static uint16_t guarded_sequence(void *opaque)
{
	struct guarded_context *guard = opaque;

	return guard->base->next_sequence(guard->base->opaque);
}

static int setup_guarded_ops(const struct orbi_paths *paths,
			     const struct controller_session *session,
			     const struct setup_ops *base,
			     struct guarded_context *context,
			     struct setup_ops *guarded,
			     struct orbi_error *error)
{
	if (base == NULL || base->set_hci_power == NULL ||
	    base->confirm_hci_down == NULL || base->csr_command == NULL ||
	    base->sync_storage == NULL || base->next_sequence == NULL) {
		orbi_error_set(error, "setup operation is unavailable");
		return -1;
	}
	context->paths = paths;
	context->session = session;
	context->base = base;
	guarded->set_hci_power = guarded_power;
	guarded->confirm_hci_down = guarded_confirm_down;
	guarded->csr_command = guarded_csr;
	guarded->sync_storage = guarded_sync;
	guarded->next_sequence = guarded_sequence;
	guarded->opaque = context;
	return 0;
}

int orbi_prepare(const struct orbi_paths *paths, const struct setup_ops *ops,
		 struct orbi_error *error)
{
	struct controller_session session;
	struct pending_transaction pending;
	struct guarded_context context;
	struct setup_ops guarded;
	uint16_t sequence;
	uint16_t status;
	uint16_t value;

	if (orbi_require_no_pending(paths->pending, error) < 0 ||
	    orbi_require_hciextn(paths->hciextn, error) < 0 ||
	    orbi_attached_session(paths, &session, error) < 0 ||
	    setup_guarded_ops(paths, &session, ops,
			      &context, &guarded, error) < 0)
		return -1;
	if (guarded.set_hci_power(session.hci.name, true,
				 guarded.opaque, error) < 0)
		return -1;
	sequence = guarded.next_sequence(guarded.opaque);
	/*
	 * The initial GET proves the complete CSR response identity. Its status
	 * and current value intentionally do not suppress the required SET.
	 */
	if (orbi_csr_read_ana(session.hci.name, sequence,
			      &status, &value, &guarded, error) < 0)
		return -1;
	guarded.sync_storage(guarded.opaque);
	memset(&pending, 0, sizeof(pending));
	pending.uart = session.uart;
	pending.post_sequence = (uint16_t) (sequence + 4);
	if (orbi_write_pending(paths, &pending, error) < 0)
		return -1;
	if (orbi_csr_set_ana(session.hci.name, (uint16_t) (sequence + 1),
			     &guarded, error) < 0) {
		char cause[ORBI_ERROR_MAX];

		(void) snprintf(cause, sizeof(cause), "%s", error->message);
		orbi_error_set(error,
			       "ANA_FREQ transaction did not finish; reboot before retrying: %s",
			       cause);
		return -1;
	}
	return 0;
}

int orbi_verify(const struct orbi_paths *paths, const struct setup_ops *ops,
		struct orbi_error *error)
{
	struct controller_session session;
	struct pending_transaction pending;
	struct guarded_context context;
	struct setup_ops guarded;
	uint16_t status;
	uint16_t value;

	if (orbi_read_pending(paths->pending, &pending, error) < 0 ||
	    orbi_attached_session(paths, &session, error) < 0 ||
	    orbi_verify_pending_uart(&pending, &session.uart, error) < 0 ||
	    orbi_require_hciextn(paths->hciextn, error) < 0 ||
	    setup_guarded_ops(paths, &session, ops,
			      &context, &guarded, error) < 0)
		return -1;
	if (guarded.set_hci_power(session.hci.name, true,
				 guarded.opaque, error) < 0 ||
	    orbi_csr_read_ana(session.hci.name, pending.post_sequence,
			      &status, &value, &guarded, error) < 0)
		return -1;
	if (status != 0 || value != 26000) {
		orbi_error_set(error,
			       "post-warm ANA_FREQ is status=%u value=%u",
			       status, value);
		return -1;
	}
	if (guarded.set_hci_power(session.hci.name, false,
				 guarded.opaque, error) < 0 ||
	    guarded.confirm_hci_down(session.hci.name,
				     guarded.opaque, error) < 0)
		return -1;
	return orbi_clear_pending(paths->pending, error);
}

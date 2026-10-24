/* SPDX-License-Identifier: GPL-2.0-only */

#include "orbi_bt_setup.h"

#include <dirent.h>
#include <endian.h>
#include <errno.h>
#include <fcntl.h>
#include <stdbool.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/sysmacros.h>
#include <unistd.h>

static int copy_text(char *destination, size_t capacity, const char *source,
		     struct orbi_error *error)
{
	int length;

	length = snprintf(destination, capacity, "%s", source);
	if (length < 0 || (size_t) length >= capacity) {
		orbi_error_set(error, "path or identity is too long");
		return -1;
	}
	return 0;
}

static int join_path(char *destination, size_t capacity, const char *left,
		     const char *right, struct orbi_error *error)
{
	int length;

	length = snprintf(destination, capacity, "%s/%s", left, right);
	if (length < 0 || (size_t) length >= capacity) {
		orbi_error_set(error, "path is too long");
		return -1;
	}
	return 0;
}

static int relative_to_root(const char *root, const char *path,
			    const char **relative, struct orbi_error *error)
{
	size_t root_length = strlen(root);
	size_t path_length = strlen(path);

	if (path_length <= root_length ||
	    strncmp(root, path, root_length) != 0 ||
	    path[root_length] != '/') {
		orbi_error_set(error, "path escapes its configured root: %s", path);
		return -1;
	}
	*relative = path + root_length + 1;
	return 0;
}

bool orbi_path_beneath(const char *path, const char *parent)
{
	size_t parent_length = strlen(parent);
	size_t path_length = strlen(path);

	return path_length > parent_length + 1 &&
	       strncmp(path, parent, parent_length) == 0 &&
	       path[parent_length] == '/' && path[parent_length + 1] != '\0';
}

static int root_fd(const char *root, struct orbi_error *error)
{
	int fd = open(root, O_PATH | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW);

	if (fd < 0)
		orbi_error_set(error, "open root %s: %s", root, strerror(errno));
	return fd;
}

static int canonical_path(const char *path, const char *root, char *output,
			  size_t capacity, struct orbi_error *error)
{
	char resolved[PATH_MAX];

	if (realpath(path, resolved) == NULL) {
		orbi_error_set(error, "resolve %s: %s", path, strerror(errno));
		return -1;
	}
	if (strcmp(resolved, root) != 0 && !orbi_path_beneath(resolved, root)) {
		orbi_error_set(error, "resolved path escapes %s: %s", root, resolved);
		return -1;
	}
	return copy_text(output, capacity, resolved, error);
}

static int directory_name(const char *path, char *output, size_t capacity,
			  struct orbi_error *error)
{
	const char *slash = strrchr(path, '/');
	size_t length;

	if (slash == NULL || slash == path) {
		orbi_error_set(error, "path has no parent: %s", path);
		return -1;
	}
	length = (size_t) (slash - path);
	if (length >= capacity) {
		orbi_error_set(error, "parent path is too long");
		return -1;
	}
	memcpy(output, path, length);
	output[length] = '\0';
	return 0;
}

static int resolve_link(int fd, const char *root, const char *link_path,
			char *output, size_t capacity,
			struct orbi_error *error)
{
	const char *relative;
	struct stat status;
	char target[PATH_MAX];
	char parent[PATH_MAX];
	char candidate[PATH_MAX];
	ssize_t length;

	if (relative_to_root(root, link_path, &relative, error) < 0)
		return -1;
	if (fstatat(fd, relative, &status, AT_SYMLINK_NOFOLLOW) < 0) {
		orbi_error_set(error, "inspect symlink %s: %s",
			       link_path, strerror(errno));
		return -1;
	}
	if (!S_ISLNK(status.st_mode)) {
		orbi_error_set(error, "expected symlink: %s", link_path);
		return -1;
	}
	length = readlinkat(fd, relative, target, sizeof(target) - 1);
	if (length < 0) {
		orbi_error_set(error, "read symlink %s: %s",
			       link_path, strerror(errno));
		return -1;
	}
	if ((size_t) length >= sizeof(target) - 1) {
		orbi_error_set(error, "symlink target is too long: %s", link_path);
		return -1;
	}
	target[length] = '\0';
	if (target[0] == '/') {
		if (copy_text(candidate, sizeof(candidate), target, error) < 0)
			return -1;
	} else {
		if (directory_name(link_path, parent, sizeof(parent), error) < 0)
			return -1;
		if (join_path(candidate, sizeof(candidate), parent, target, error) < 0)
			return -1;
	}
	return canonical_path(candidate, root, output, capacity, error);
}

static int link_exists(int fd, const char *root, const char *link_path,
		       bool *exists, struct orbi_error *error)
{
	const char *relative;
	struct stat status;

	if (relative_to_root(root, link_path, &relative, error) < 0)
		return -1;
	if (fstatat(fd, relative, &status, AT_SYMLINK_NOFOLLOW) < 0) {
		if (errno == ENOENT) {
			*exists = false;
			return 0;
		}
		orbi_error_set(error, "inspect symlink %s: %s",
			       link_path, strerror(errno));
		return -1;
	}
	if (!S_ISLNK(status.st_mode)) {
		orbi_error_set(error, "expected symlink: %s", link_path);
		return -1;
	}
	*exists = true;
	return 0;
}

static int read_file_at_root(int fd, const char *root, const char *path,
			     uint8_t *buffer, size_t capacity, size_t *length,
			     struct orbi_error *error)
{
	const char *relative;
	ssize_t result;
	size_t used = 0;
	int file;

	if (relative_to_root(root, path, &relative, error) < 0)
		return -1;
	file = openat(fd, relative, O_RDONLY | O_CLOEXEC | O_NOFOLLOW);
	if (file < 0) {
		orbi_error_set(error, "open %s: %s", path, strerror(errno));
		return -1;
	}
	while (used < capacity) {
		result = read(file, buffer + used, capacity - used);
		if (result < 0 && errno == EINTR)
			continue;
		if (result < 0) {
			orbi_error_set(error, "read %s: %s", path, strerror(errno));
			close(file);
			return -1;
		}
		if (result == 0)
			break;
		used += (size_t) result;
	}
	if (used == capacity) {
		uint8_t extra;

		do {
			result = read(file, &extra, 1);
		} while (result < 0 && errno == EINTR);
		if (result != 0) {
			orbi_error_set(error, "file is too large: %s", path);
			close(file);
			return -1;
		}
	}
	if (close(file) < 0) {
		orbi_error_set(error, "close %s: %s", path, strerror(errno));
		return -1;
	}
	*length = used;
	return 0;
}

static bool compatible_contains(const uint8_t *buffer, size_t length,
				const char *expected)
{
	size_t offset = 0;
	size_t expected_length = strlen(expected);

	while (offset < length) {
		size_t item_length = strnlen((const char *) buffer + offset,
					    length - offset);

		if (item_length == expected_length &&
		    memcmp(buffer + offset, expected, item_length) == 0)
			return true;
		if (item_length == length - offset)
			break;
		offset += item_length + 1;
	}
	return false;
}

static bool tty_name_valid(const char *name)
{
	const char prefix[] = "ttyMSM";
	size_t length = strlen(name);
	size_t index;

	if (length <= sizeof(prefix) - 1 ||
	    strncmp(name, prefix, sizeof(prefix) - 1) != 0)
		return false;
	for (index = sizeof(prefix) - 1; name[index] != '\0'; index++)
		if (name[index] < '0' || name[index] > '9')
			return false;
	return true;
}

static bool hci_name_valid(const char *name)
{
	size_t length = strlen(name);
	size_t index;

	if (length <= 3 || strncmp(name, "hci", 3) != 0)
		return false;
	for (index = 3; name[index] != '\0'; index++)
		if (name[index] < '0' || name[index] > '9')
			return false;
	return true;
}

static int path_parent(char *path, const char *root,
		       struct orbi_error *error)
{
	char *slash = strrchr(path, '/');

	if (slash == NULL || slash == path || strcmp(path, root) == 0) {
		orbi_error_set(error, "UART ancestry left sysfs before msm_serial");
		return -1;
	}
	*slash = '\0';
	if (!orbi_path_beneath(path, root)) {
		orbi_error_set(error, "UART ancestry escaped sysfs");
		return -1;
	}
	return 0;
}

static int tty_platform(int fd, const char *root, const char *node,
			const char *tty_path, const char *device_path,
			char *platform, size_t capacity,
			struct orbi_error *error)
{
	char current[PATH_MAX];
	char link[PATH_MAX];
	char of_node[PATH_MAX];
	char driver[PATH_MAX];
	char subsystem[PATH_MAX];
	char platform_driver[PATH_MAX];
	char port_driver[PATH_MAX];
	char ctrl_driver[PATH_MAX];
	char platform_subsystem[PATH_MAX];
	char serial_subsystem[PATH_MAX];
	char platform_devices[PATH_MAX];
	unsigned int layer;
	bool serial_paths_ready = false;
	bool exists;

	if (!orbi_path_beneath(tty_path, root) ||
	    !orbi_path_beneath(device_path, root)) {
		orbi_error_set(error, "Bluetooth TTY device escapes sysfs");
		return -1;
	}
	if (copy_text(current, sizeof(current), device_path, error) < 0)
		return -1;
	if (join_path(link, sizeof(link), root,
		      "bus/platform/drivers/msm_serial", error) < 0 ||
	    canonical_path(link, root, platform_driver,
			   sizeof(platform_driver), error) < 0 ||
	    join_path(link, sizeof(link), root, "bus/platform", error) < 0 ||
	    canonical_path(link, root, platform_subsystem,
			   sizeof(platform_subsystem), error) < 0 ||
	    join_path(link, sizeof(link), root, "devices/platform", error) < 0 ||
	    canonical_path(link, root, platform_devices,
			   sizeof(platform_devices), error) < 0)
		return -1;

	for (layer = 0; layer < 3; layer++) {
		if (join_path(link, sizeof(link), current, "of_node", error) < 0 ||
		    link_exists(fd, root, link, &exists, error) < 0)
			return -1;
		if (!exists) {
			if (layer == 0)
				return 0;
			orbi_error_set(error,
				       "UART parent lacks a device-tree identity");
			return -1;
		}
		if (resolve_link(fd, root, link, of_node,
				 sizeof(of_node), error) < 0)
			return -1;
		if (strcmp(of_node, node) != 0) {
			if (layer == 0)
				return 0;
			orbi_error_set(error,
				       "UART ancestry changed device-tree identity");
			return -1;
		}
		if (join_path(link, sizeof(link), current, "driver", error) < 0 ||
		    resolve_link(fd, root, link, driver,
				 sizeof(driver), error) < 0 ||
		    join_path(link, sizeof(link), current, "subsystem", error) < 0 ||
		    resolve_link(fd, root, link, subsystem,
				 sizeof(subsystem), error) < 0)
			return -1;

		if (layer == 0 && strcmp(driver, platform_driver) == 0) {
			if (strcmp(subsystem, platform_subsystem) != 0 ||
			    !orbi_path_beneath(current, platform_devices) ||
			    !orbi_path_beneath(tty_path, current)) {
				orbi_error_set(error,
					       "invalid msm_serial platform owner");
				return -1;
			}
			return copy_text(platform, capacity, current, error) < 0 ?
				-1 : 1;
		}
		if (layer == 0 && !serial_paths_ready) {
			if (join_path(link, sizeof(link), root,
				      "bus/serial-base/drivers/port", error) < 0 ||
			    canonical_path(link, root, port_driver,
					   sizeof(port_driver), error) < 0 ||
			    join_path(link, sizeof(link), root,
				      "bus/serial-base/drivers/ctrl", error) < 0 ||
			    canonical_path(link, root, ctrl_driver,
					   sizeof(ctrl_driver), error) < 0 ||
			    join_path(link, sizeof(link), root,
				      "bus/serial-base", error) < 0 ||
			    canonical_path(link, root, serial_subsystem,
					   sizeof(serial_subsystem), error) < 0)
				return -1;
			serial_paths_ready = true;
		}
		if (layer == 0 &&
		    (strcmp(driver, port_driver) != 0 ||
		     strcmp(subsystem, serial_subsystem) != 0)) {
			orbi_error_set(error,
				       "unexpected serial-base port identity");
			return -1;
		}
		if (layer == 1 &&
		    (strcmp(driver, ctrl_driver) != 0 ||
		     strcmp(subsystem, serial_subsystem) != 0)) {
			orbi_error_set(error,
				       "unexpected serial-base ctrl identity");
			return -1;
		}
		if (layer == 2) {
			if (strcmp(driver, platform_driver) != 0 ||
			    strcmp(subsystem, platform_subsystem) != 0 ||
			    !orbi_path_beneath(current, platform_devices) ||
			    !orbi_path_beneath(tty_path, current)) {
				orbi_error_set(error,
					       "serial-base ancestry did not reach msm_serial");
				return -1;
			}
			return copy_text(platform, capacity, current, error) < 0 ?
				-1 : 1;
		}
		if (path_parent(current, root, error) < 0)
			return -1;
	}
	orbi_error_set(error, "UART ancestry did not reach msm_serial");
	return -1;
}

static int read_tty_numbers(int fd, const char *root, const char *tty_path,
			    unsigned int *major_number,
			    unsigned int *minor_number,
			    struct orbi_error *error)
{
	uint8_t buffer[64];
	char path[PATH_MAX];
	char text[65];
	char trailing;
	size_t length;
	unsigned int major_value;
	unsigned int minor_value;

	if (join_path(path, sizeof(path), tty_path, "dev", error) < 0 ||
	    read_file_at_root(fd, root, path, buffer,
			      sizeof(buffer), &length, error) < 0)
		return -1;
	if (length == 0 || length >= sizeof(text)) {
		orbi_error_set(error, "Bluetooth TTY has an invalid device number");
		return -1;
	}
	memcpy(text, buffer, length);
	text[length] = '\0';
	if (sscanf(text, "%u:%u %c", &major_value, &minor_value, &trailing) != 2) {
		orbi_error_set(error, "Bluetooth TTY has an invalid device number");
		return -1;
	}
	*major_number = major_value;
	*minor_number = minor_value;
	return 0;
}

int orbi_discover_uart(const struct orbi_paths *paths,
		       struct uart_identity *uart, struct orbi_error *error)
{
	uint8_t buffer[256];
	uint32_t address;
	uint32_t size;
	char root[PATH_MAX];
	char tree[PATH_MAX];
	char node_path[PATH_MAX];
	char node[PATH_MAX];
	char class_path[PATH_MAX];
	char entry_path[PATH_MAX];
	char tty_path[PATH_MAX];
	char device_link[PATH_MAX];
	char device_path[PATH_MAX];
	char platform[PATH_MAX];
	struct uart_identity selected;
	struct stat status;
	DIR *directory = NULL;
	struct dirent *entry;
	size_t length;
	int fd = -1;
	int directory_fd = -1;
	bool found = false;
	int result = -1;

	memset(&selected, 0, sizeof(selected));
	if (canonical_path(paths->sysfs, paths->sysfs, root,
			   sizeof(root), error) < 0)
		return -1;
	fd = root_fd(root, error);
	if (fd < 0)
		return -1;
	if (join_path(tree, sizeof(tree), root,
		      "firmware/devicetree/base", error) < 0 ||
	    join_path(node_path, sizeof(node_path), tree,
		      "soc/serial@78b0000", error) < 0 ||
	    canonical_path(node_path, root, node, sizeof(node), error) < 0)
		goto out;
	if (join_path(entry_path, sizeof(entry_path), tree, "compatible", error) < 0 ||
	    read_file_at_root(fd, root, entry_path, buffer,
			      sizeof(buffer), &length, error) < 0)
		goto out;
	if (!compatible_contains(buffer, length, "netgear,rbr50") &&
	    !compatible_contains(buffer, length, "netgear,rbs50")) {
		orbi_error_set(error,
			       "only Orbi RBR50/RBS50 v1 is supported");
		goto out;
	}
	if (join_path(entry_path, sizeof(entry_path), node, "compatible", error) < 0 ||
	    read_file_at_root(fd, root, entry_path, buffer,
			      sizeof(buffer), &length, error) < 0)
		goto out;
	if (!compatible_contains(buffer, length, "qcom,msm-uartdm-v1.4")) {
		orbi_error_set(error,
			       "Bluetooth UART has an unexpected compatible");
		goto out;
	}
	if (join_path(entry_path, sizeof(entry_path), node, "reg", error) < 0 ||
	    read_file_at_root(fd, root, entry_path, buffer,
			      sizeof(buffer), &length, error) < 0)
		goto out;
	if (length != 8) {
		orbi_error_set(error, "Bluetooth UART has invalid registers");
		goto out;
	}
	memcpy(&address, buffer, sizeof(address));
	memcpy(&size, buffer + sizeof(address), sizeof(size));
	if (be32toh(address) != 0x078b0000U || be32toh(size) != 0x200U) {
		orbi_error_set(error, "Bluetooth UART has unexpected registers");
		goto out;
	}
	if (join_path(entry_path, sizeof(entry_path), node, "status", error) < 0 ||
	    read_file_at_root(fd, root, entry_path, buffer,
			      sizeof(buffer) - 1, &length, error) < 0)
		goto out;
	buffer[length] = '\0';
	while (length > 0 &&
	       (buffer[length - 1] == '\0' || buffer[length - 1] == '\n'))
		buffer[--length] = '\0';
	if (strcmp((const char *) buffer, "okay") != 0 &&
	    strcmp((const char *) buffer, "ok") != 0) {
		orbi_error_set(error,
			       "Bluetooth UART is not enabled by the device tree");
		goto out;
	}

	if (join_path(class_path, sizeof(class_path), root, "class/tty", error) < 0)
		goto out;
	directory_fd = openat(fd, "class/tty",
			      O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW);
	if (directory_fd < 0) {
		orbi_error_set(error, "open TTY class: %s", strerror(errno));
		goto out;
	}
	directory = fdopendir(directory_fd);
	if (directory == NULL) {
		orbi_error_set(error, "read TTY class: %s", strerror(errno));
		goto out;
	}
	directory_fd = -1;
	for (;;) {
		int match;

		errno = 0;
		entry = readdir(directory);
		if (entry == NULL) {
			if (errno != 0) {
				orbi_error_set(error, "read TTY class: %s",
					       strerror(errno));
				goto out;
			}
			break;
		}
		if (!tty_name_valid(entry->d_name))
			continue;
		if (join_path(entry_path, sizeof(entry_path),
			      class_path, entry->d_name, error) < 0 ||
		    resolve_link(fd, root, entry_path, tty_path,
				 sizeof(tty_path), error) < 0 ||
		    join_path(device_link, sizeof(device_link),
			      entry_path, "device", error) < 0 ||
		    resolve_link(fd, root, device_link, device_path,
				 sizeof(device_path), error) < 0)
			goto out;
		match = tty_platform(fd, root, node, tty_path, device_path,
				     platform, sizeof(platform), error);
		if (match < 0)
			goto out;
		if (match == 0)
			continue;
		if (found) {
			orbi_error_set(error,
				       "Bluetooth UART has multiple TTY candidates");
			goto out;
		}
		if (stat(tty_path, &status) < 0) {
			orbi_error_set(error, "stat Bluetooth TTY: %s",
				       strerror(errno));
			goto out;
		}
		if (copy_text(selected.name, sizeof(selected.name),
			      entry->d_name, error) < 0 ||
		    copy_text(selected.tty_path, sizeof(selected.tty_path),
			      tty_path, error) < 0 ||
		    copy_text(selected.platform_path,
			      sizeof(selected.platform_path),
			      platform, error) < 0 ||
		    read_tty_numbers(fd, root, tty_path,
				     &selected.major_number,
				     &selected.minor_number, error) < 0)
			goto out;
		selected.stat_device = status.st_dev;
		selected.inode = status.st_ino;
		found = true;
	}
	if (!found) {
		orbi_error_set(error,
			       "enabled Bluetooth UART has no unique ttyMSM device");
		goto out;
	}
	*uart = selected;
	result = 0;

out:
	if (directory != NULL)
		closedir(directory);
	else if (directory_fd >= 0)
		close(directory_fd);
	close(fd);
	return result;
}

static int stat_hci_entry(const char *entry_path, struct hci_identity *hci,
			  const char *name, struct orbi_error *error)
{
	struct stat status;

	if (stat(entry_path, &status) < 0) {
		orbi_error_set(error, "stat HCI controller: %s", strerror(errno));
		return -1;
	}
	if (copy_text(hci->name, sizeof(hci->name), name, error) < 0)
		return -1;
	hci->stat_device = status.st_dev;
	hci->inode = status.st_ino;
	return 0;
}

int orbi_find_hci(const struct orbi_paths *paths,
		  const struct uart_identity *uart, struct hci_identity *hci,
		  struct orbi_error *error)
{
	char root[PATH_MAX];
	char class_path[PATH_MAX];
	char entry_path[PATH_MAX];
	char resolved[PATH_MAX];
	struct hci_identity selected;
	DIR *directory = NULL;
	struct dirent *entry;
	int fd = -1;
	int directory_fd = -1;
	bool found = false;
	int result = -1;

	memset(&selected, 0, sizeof(selected));
	if (canonical_path(paths->sysfs, paths->sysfs, root,
			   sizeof(root), error) < 0)
		return -1;
	fd = root_fd(root, error);
	if (fd < 0)
		return -1;
	if (join_path(class_path, sizeof(class_path), root,
		      "class/bluetooth", error) < 0)
		goto out;
	directory_fd = openat(fd, "class/bluetooth",
			      O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW);
	if (directory_fd < 0) {
		if (errno == ENOENT) {
			memset(hci, 0, sizeof(*hci));
			result = 0;
			goto out;
		}
		orbi_error_set(error, "open Bluetooth class: %s", strerror(errno));
		goto out;
	}
	directory = fdopendir(directory_fd);
	if (directory == NULL) {
		orbi_error_set(error, "read Bluetooth class: %s", strerror(errno));
		goto out;
	}
	directory_fd = -1;
	for (;;) {
		errno = 0;
		entry = readdir(directory);
		if (entry == NULL) {
			if (errno != 0) {
				orbi_error_set(error, "read Bluetooth class: %s",
					       strerror(errno));
				goto out;
			}
			break;
		}
		if (!hci_name_valid(entry->d_name))
			continue;
		if (join_path(entry_path, sizeof(entry_path),
			      class_path, entry->d_name, error) < 0 ||
		    resolve_link(fd, root, entry_path, resolved,
				 sizeof(resolved), error) < 0)
			goto out;
		if (!orbi_path_beneath(resolved, uart->tty_path))
			continue;
		if (found) {
			orbi_error_set(error,
				       "multiple HCI controllers belong to the UART");
			goto out;
		}
		if (stat_hci_entry(entry_path, &selected,
				   entry->d_name, error) < 0)
			goto out;
		found = true;
	}
	if (found)
		*hci = selected;
	else
		memset(hci, 0, sizeof(*hci));
	result = 0;

out:
	if (directory != NULL)
		closedir(directory);
	else if (directory_fd >= 0)
		close(directory_fd);
	close(fd);
	return result;
}

static bool numeric_name(const char *name)
{
	size_t index;

	if (name[0] == '\0')
		return false;
	for (index = 0; name[index] != '\0'; index++)
		if (name[index] < '0' || name[index] > '9')
			return false;
	return true;
}

static int refuse_open_device(const char *proc, dev_t device,
			      struct orbi_error *error)
{
	DIR *processes = NULL;
	DIR *descriptors = NULL;
	struct dirent *process;
	struct dirent *descriptor;
	struct stat status;
	char fd_path[PATH_MAX];
	int process_fd = -1;
	int descriptors_fd = -1;
	int result = -1;

	process_fd = open(proc, O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW);
	if (process_fd < 0) {
		orbi_error_set(error, "open procfs: %s", strerror(errno));
		return -1;
	}
	processes = fdopendir(process_fd);
	if (processes == NULL) {
		orbi_error_set(error, "read procfs: %s", strerror(errno));
		close(process_fd);
		return -1;
	}
	process_fd = -1;
	for (;;) {
		errno = 0;
		process = readdir(processes);
		if (process == NULL) {
			if (errno != 0) {
				orbi_error_set(error, "read procfs: %s",
					       strerror(errno));
				goto out;
			}
			break;
		}
		if (!numeric_name(process->d_name))
			continue;
		if (snprintf(fd_path, sizeof(fd_path), "%s/fd",
			     process->d_name) >= (int) sizeof(fd_path)) {
			orbi_error_set(error, "procfs path is too long");
			goto out;
		}
		descriptors_fd = openat(dirfd(processes), fd_path,
				       O_RDONLY | O_DIRECTORY | O_CLOEXEC |
				       O_NOFOLLOW);
		if (descriptors_fd < 0 && errno == ENOENT) {
			errno = 0;
			continue;
		}
		if (descriptors_fd < 0) {
			orbi_error_set(error, "open process descriptors: %s",
				       strerror(errno));
			goto out;
		}
		descriptors = fdopendir(descriptors_fd);
		if (descriptors == NULL) {
			orbi_error_set(error, "read process descriptors: %s",
				       strerror(errno));
			goto out;
		}
		descriptors_fd = -1;
		for (;;) {
			errno = 0;
			descriptor = readdir(descriptors);
			if (descriptor == NULL) {
				if (errno != 0) {
					orbi_error_set(error,
						       "read process descriptors: %s",
						       strerror(errno));
					goto out;
				}
				break;
			}
			if (!numeric_name(descriptor->d_name))
				continue;
			if (fstatat(dirfd(descriptors), descriptor->d_name,
				    &status, 0) < 0) {
				if (errno == ENOENT)
					continue;
				orbi_error_set(error, "inspect process descriptor: %s",
					       strerror(errno));
				goto out;
			}
			if (S_ISCHR(status.st_mode) && status.st_rdev == device) {
				orbi_error_set(error,
					       "Bluetooth UART is already open");
				goto out;
			}
		}
		closedir(descriptors);
		descriptors = NULL;
	}
	result = 0;

out:
	if (descriptors != NULL)
		closedir(descriptors);
	else if (descriptors_fd >= 0)
		close(descriptors_fd);
	closedir(processes);
	return result;
}

static int console_uses_tty(const char *proc, const char *tty,
			    struct orbi_error *error)
{
	char path[PATH_MAX];
	char line[256];
	FILE *file;

	if (snprintf(path, sizeof(path), "%s/consoles", proc) >=
	    (int) sizeof(path)) {
		orbi_error_set(error, "procfs consoles path is too long");
		return -1;
	}
	file = fopen(path, "r");
	if (file == NULL) {
		orbi_error_set(error, "open consoles: %s", strerror(errno));
		return -1;
	}
	while (fgets(line, sizeof(line), file) != NULL) {
		char *space = strpbrk(line, " \t");

		if (space != NULL)
			*space = '\0';
		if (strcmp(line, tty) == 0) {
			fclose(file);
			orbi_error_set(error, "Bluetooth UART is a console");
			return -1;
		}
	}
	if (ferror(file)) {
		orbi_error_set(error, "read consoles: %s", strerror(errno));
		fclose(file);
		return -1;
	}
	if (fclose(file) < 0) {
		orbi_error_set(error, "close consoles: %s", strerror(errno));
		return -1;
	}
	return 0;
}

int orbi_available_uart(const struct orbi_paths *paths,
			struct uart_identity *uart, struct orbi_error *error)
{
	struct hci_identity hci;
	struct stat status;
	char device_path[PATH_MAX];

	if (orbi_discover_uart(paths, uart, error) < 0 ||
	    orbi_find_hci(paths, uart, &hci, error) < 0)
		return -1;
	if (hci.name[0] != '\0') {
		orbi_error_set(error,
			       "Bluetooth UART already has an attached controller");
		return -1;
	}
	if (console_uses_tty(paths->proc, uart->name, error) < 0)
		return -1;
	if (join_path(device_path, sizeof(device_path),
		      paths->dev, uart->name, error) < 0)
		return -1;
	if (lstat(device_path, &status) < 0) {
		orbi_error_set(error, "inspect Bluetooth UART device: %s",
			       strerror(errno));
		return -1;
	}
	if (S_ISLNK(status.st_mode) || !S_ISCHR(status.st_mode)) {
		orbi_error_set(error,
			       "Bluetooth UART device is not a direct character device");
		return -1;
	}
	if (major(status.st_rdev) != uart->major_number ||
	    minor(status.st_rdev) != uart->minor_number) {
		orbi_error_set(error,
			       "Bluetooth UART device does not match sysfs");
		return -1;
	}
	return refuse_open_device(paths->proc, status.st_rdev, error);
}

int orbi_attached_session(const struct orbi_paths *paths,
			  struct controller_session *session,
			  struct orbi_error *error)
{
	if (orbi_discover_uart(paths, &session->uart, error) < 0 ||
	    orbi_find_hci(paths, &session->uart, &session->hci, error) < 0)
		return -1;
	if (session->hci.name[0] == '\0') {
		orbi_error_set(error,
			       "verified UART has no attached HCI controller");
		return -1;
	}
	return 0;
}

int orbi_verify_session(const struct orbi_paths *paths,
			const struct controller_session *session,
			struct orbi_error *error)
{
	struct controller_session current;

	if (orbi_attached_session(paths, &current, error) < 0)
		return -1;
	if (strcmp(current.uart.name, session->uart.name) != 0 ||
	    strcmp(current.uart.tty_path, session->uart.tty_path) != 0 ||
	    strcmp(current.uart.platform_path,
		   session->uart.platform_path) != 0 ||
	    current.uart.stat_device != session->uart.stat_device ||
	    current.uart.inode != session->uart.inode ||
	    strcmp(current.hci.name, session->hci.name) != 0 ||
	    current.hci.stat_device != session->hci.stat_device ||
	    current.hci.inode != session->hci.inode) {
		orbi_error_set(error, "UART or HCI controller identity changed");
		return -1;
	}
	return 0;
}

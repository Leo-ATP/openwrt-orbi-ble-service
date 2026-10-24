# SPDX-License-Identifier: GPL-2.0-only

CC ?= cc
STRIP ?= strip
CPPFLAGS ?=
CFLAGS ?= -O2
LDFLAGS ?=

CPPFLAGS += -Iinclude -D_GNU_SOURCE
WARNINGS = -Wall -Wextra -Werror -Wconversion -Wformat=2 \
	-Wshadow -Wstrict-prototypes -Wmissing-prototypes \
	-Wpointer-arith -Wcast-qual -Wwrite-strings
HARDEN_CFLAGS = -fstack-protector-strong -fPIE
HARDEN_LDFLAGS = -Wl,-z,relro,-z,now -pie

PROGRAM = build/orbi-bt-setup
TEST_PROGRAM = build/test-orbi-bt-setup
PRODUCTION_SOURCES = src/main.c src/setup.c src/sysfs.c src/csr.c
LIBRARY_SOURCES = src/setup.c src/sysfs.c src/csr.c
TEST_SOURCES = tests/test_main.c

.PHONY: all clean test sanitize analyze strip

all: $(PROGRAM)

build:
	mkdir -p $@

$(PROGRAM): $(PRODUCTION_SOURCES) include/orbi_bt_setup.h | build
	$(CC) $(CPPFLAGS) $(CFLAGS) $(WARNINGS) $(HARDEN_CFLAGS) \
		$(PRODUCTION_SOURCES) $(LDFLAGS) $(HARDEN_LDFLAGS) -o $@

$(TEST_PROGRAM): $(LIBRARY_SOURCES) $(TEST_SOURCES) include/orbi_bt_setup.h | build
	$(CC) $(CPPFLAGS) $(CFLAGS) $(WARNINGS) $(HARDEN_CFLAGS) \
		-DORBI_BT_TESTING $(LIBRARY_SOURCES) $(TEST_SOURCES) \
		$(LDFLAGS) $(HARDEN_LDFLAGS) -o $@

test: $(TEST_PROGRAM)
	$(TEST_PROGRAM)

sanitize: | build
	$(CC) $(CPPFLAGS) -O1 -g $(WARNINGS) -DORBI_BT_TESTING \
		-fno-omit-frame-pointer -fsanitize=address,undefined \
		$(LIBRARY_SOURCES) $(TEST_SOURCES) \
		-fsanitize=address,undefined -o build/test-orbi-bt-setup-sanitize
	ASAN_OPTIONS=detect_leaks=1:strict_string_checks=1 \
		UBSAN_OPTIONS=halt_on_error=1:print_stacktrace=1 \
		build/test-orbi-bt-setup-sanitize

analyze:
	$(CC) $(CPPFLAGS) -O2 $(WARNINGS) -fanalyzer -fsyntax-only \
		$(PRODUCTION_SOURCES)

strip: $(PROGRAM)
	cp $(PROGRAM) $(PROGRAM).stripped
	$(STRIP) --strip-all $(PROGRAM).stripped

clean:
	rm -rf build

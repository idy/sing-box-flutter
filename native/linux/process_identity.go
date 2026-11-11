// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

var errProcessIdentity = errors.New("invalid Linux process identity")

// Require a proc mount for the worker's PID namespace. Dart inherits the same
// mount and uses this identity, plus kill(pid, 0), to verify actual termination.
func coreProcessIdentity() (string, string, error) {
	content, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return "", "", errProcessIdentity
	}
	pid, start, err := parseProcessIdentity(string(content))
	if err != nil || pid != strconv.Itoa(os.Getpid()) {
		return "", "", errProcessIdentity
	}
	return pid, start, nil
}

func parseProcessIdentity(content string) (string, string, error) {
	opening, closing := strings.IndexByte(content, '('), strings.LastIndexByte(content, ')')
	if opening < 1 || closing < opening {
		return "", "", errProcessIdentity
	}
	pid := strings.TrimSpace(content[:opening])
	number, err := strconv.Atoi(pid)
	if err != nil || number <= 0 || strconv.Itoa(number) != pid {
		return "", "", errProcessIdentity
	}
	fields := strings.Fields(content[closing+1:])
	if len(fields) < 20 {
		return "", "", errProcessIdentity
	}
	start := fields[19] // starttime is field 22; the suffix starts at field 3.
	value, err := strconv.ParseUint(start, 10, 64)
	if err != nil || strconv.FormatUint(value, 10) != start {
		return "", "", errProcessIdentity
	}
	return pid, start, nil
}

package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Host is what the default classes are sized from.
type Host struct {
	CPUs   int
	Memory Bytes
}

// Default writes a new config file for a host: a normal class with every core
// and about three quarters of the memory, and an idle class that uses only the
// cores the normal class leaves idle. With docker, both classes run Docker
// inside their jobs.
func Default(worker, box string, h Host, docker bool) string {
	extra := ""
	if docker {
		extra = "docker = true\n"
	}
	return fmt.Sprintf(`# boxrunner configuration. The format is in docs/config.md.
# The agent reads it on start: systemctl restart boxrunner after a change.

worker = %s
box = %s
disk_floor = "10GB"

[[class]]
name = "boxrunner"
cpus = %d
memory = %q
pids = %d
shm = "1GiB"
priority = "normal"
max = 1
%s
[[class]]
name = "boxrunner-light"
cpus = %d
memory = %q
pids = %d
shm = "1GiB"
priority = "idle"
max = 2
%s`, strconv.Quote(worker), strconv.Quote(box),
		h.CPUs, share(h.Memory, 75).String(), DefaultPids, extra,
		h.CPUs, share(h.Memory, 40).String(), DefaultPids, extra)
}

// share is percent of total, rounded down to 256 MiB so the file reads well.
func share(total Bytes, percent int64) Bytes {
	step := 256 * MiB
	return max(total*Bytes(percent)/100/step*step, minMemory)
}

var tableHeader = regexp.MustCompile(`^\s*\[`)

// SetTopLevel sets a top-level string key in a config file and keeps every
// other line, comments included. A missing key is added before the first table.
func SetTopLevel(content, key, value string) string {
	line := key + " = " + strconv.Quote(value)
	keyPattern := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(key) + `\s*=`)
	lines := strings.Split(content, "\n")
	firstTable := len(lines)
	for i, l := range lines {
		if tableHeader.MatchString(l) {
			firstTable = i
			break
		}
		if keyPattern.MatchString(l) {
			lines[i] = line
			return strings.Join(lines, "\n")
		}
	}
	insert := firstTable
	for insert > 0 && strings.TrimSpace(lines[insert-1]) == "" {
		insert--
	}
	lines = append(lines[:insert], append([]string{line}, lines[insert:]...)...)
	return strings.Join(lines, "\n")
}

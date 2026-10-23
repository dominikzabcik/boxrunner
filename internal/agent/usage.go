package agent

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strconv"

	"github.com/dominikzabcik/boxrunner/internal/docker"
	"github.com/dominikzabcik/boxrunner/internal/layout"
	"github.com/dominikzabcik/boxrunner/internal/status"
)

// sampleUsage keeps the latest cgroup reading of each running job container.
// The cgroup goes away with the container, before the job is logged, so the
// last sample stands for the whole job: both values only grow.
func (a *Agent) sampleUsage(list []docker.Container) {
	for _, c := range list {
		runner := c.Labels[layout.LabelRunner]
		if runner == "" || !c.Running() {
			continue
		}
		if u, ok := readUsage(a.CgroupDir, c.ID); ok {
			a.usage.Store(runner, u)
		}
	}
}

// readUsage reads a container's cgroup, which Docker's systemd driver names
// docker-<id>.scope in the slice the container runs in.
func readUsage(cgroupDir, id string) (status.Usage, bool) {
	if id == "" {
		return status.Usage{}, false
	}
	scopes, _ := filepath.Glob(filepath.Join(cgroupDir, "*", "docker-"+id+".scope"))
	if len(scopes) != 1 {
		return status.Usage{}, false
	}
	cpu, err := os.ReadFile(filepath.Join(scopes[0], "cpu.stat"))
	if err != nil {
		return status.Usage{}, false
	}
	var u status.Usage
	sc := bufio.NewScanner(bytes.NewReader(cpu))
	for sc.Scan() {
		if key, value, ok := bytes.Cut(sc.Bytes(), []byte(" ")); ok && string(key) == "usage_usec" {
			usec, _ := strconv.ParseInt(string(value), 10, 64)
			u.CPUSeconds = float64(usec) / 1e6
		}
	}
	// memory.peak needs Linux 5.19; without it the peak stays 0.
	if peak, err := os.ReadFile(filepath.Join(scopes[0], "memory.peak")); err == nil {
		u.MemoryPeak, _ = strconv.ParseInt(string(bytes.TrimSpace(peak)), 10, 64)
	}
	return u, true
}

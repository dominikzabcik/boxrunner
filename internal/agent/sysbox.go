package agent

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"syscall"

	"github.com/dominikzabcik/boxrunner/internal/disk"
	"github.com/dominikzabcik/boxrunner/internal/docker"
)

// sweepSysbox removes what Sysbox keeps for containers Docker no longer has.
// Sysbox frees a container's directories when the container is removed, but
// it misses some (13 of 198 job containers in two days on one box), and each
// holds a job's whole inner /var/lib/docker, so the box ran out of disk with
// no job running. The directories are read before the containers: Docker
// creates a container before Sysbox makes its directories, so one that starts
// in between keeps them.
func (a *Agent) sweepSysbox(ctx context.Context) {
	dirs, _ := filepath.Glob(filepath.Join(a.SysboxDir, "*", "*"))
	if len(dirs) == 0 {
		return
	}
	list, err := a.Docker.List(ctx, "", false)
	if err != nil {
		return
	}
	for _, dir := range orphaned(dirs, list) {
		size := disk.Used(dir)
		if err := os.RemoveAll(dir); err != nil {
			a.Log.Warn("removing what Sysbox kept for a removed container", "dir", dir, "err", err)
			continue
		}
		a.Log.Info("removed what Sysbox kept for a removed container", "dir", dir, "bytes", size)
	}
}

var containerID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// orphaned returns the directories named after a container id that no
// container in list has. A directory that is a mount point is left alone:
// removing it would reach into whatever is mounted there.
func orphaned(dirs []string, list []docker.Container) []string {
	known := make(map[string]bool, len(list))
	for _, c := range list {
		known[c.ID] = true
	}
	var out []string
	for _, dir := range dirs {
		id := filepath.Base(dir)
		if containerID.MatchString(id) && !known[id] && !mountPoint(dir) {
			out = append(out, dir)
		}
	}
	return out
}

func mountPoint(dir string) bool {
	var self, parent syscall.Stat_t
	if syscall.Lstat(dir, &self) != nil || syscall.Lstat(filepath.Dir(dir), &parent) != nil {
		return true
	}
	return self.Mode&syscall.S_IFMT != syscall.S_IFDIR || self.Dev != parent.Dev
}

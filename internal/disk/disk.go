// Package disk reads free and used space on filesystems.
package disk

import (
	"io/fs"
	"path/filepath"
	"syscall"
)

// Free is the space unprivileged writers can still use on path's filesystem.
func Free(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// Lowest returns the least free space among the filesystems that hold paths,
// and the path on it.
func Lowest(paths ...string) (int64, string, error) {
	least, at := int64(-1), ""
	for _, p := range paths {
		free, err := Free(p)
		if err != nil {
			return 0, p, err
		}
		if least < 0 || free < least {
			least, at = free, p
		}
	}
	return least, at, nil
}

// Used is the size of the regular files under dir, as Docker counts what a
// container wrote; 0 when dir is missing.
func Used(dir string) int64 {
	var total int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// Package disk reads free space on a filesystem.
package disk

import "syscall"

// Free is the space unprivileged writers can still use on path's filesystem.
func Free(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

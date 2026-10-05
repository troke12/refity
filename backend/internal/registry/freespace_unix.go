//go:build unix

package registry

import "syscall"

// freeBytes returns the bytes available to unprivileged users on the filesystem holding dir.
func freeBytes(dir string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

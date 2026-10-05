//go:build !unix

package registry

// freeBytes is not implemented here; -1 disables the SPOOL_MIN_FREE_BYTES floor on this platform.
func freeBytes(dir string) int64 { return -1 }

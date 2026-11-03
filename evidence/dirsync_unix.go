//go:build !windows

package evidence

import "os"

// syncDir flushes a directory's directory-entry so a just-rename'd blob survives
// a crash. POSIX only; on Windows the equivalent (FlushFileBuffers on a directory
// handle) is not supported and returns ERROR_ACCESS_DENIED — see dirsync_windows.go.
func syncDir(d *os.File) error {
	return d.Sync()
}

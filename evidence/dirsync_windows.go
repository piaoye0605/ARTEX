//go:build windows

package evidence

import "os"

// syncDir is a no-op on Windows: FlushFileBuffers does not accept directory
// handles and returns ERROR_ACCESS_DENIED (verified: evidence tests all fail on
// "sync ...\evidence\blobs\NN: Access is denied."). Durability of the blob
// content itself is already guaranteed by the file-level f.Sync() in store.go
// before the rename; what is lost here is only directory-entry durability, i.e.
// a hard crash right after Put may drop the directory registration of one newly
// written blob. Acceptable trade-off for making evidence storage work at all on
// Windows deployments.
func syncDir(*os.File) error {
	return nil
}

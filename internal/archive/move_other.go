//go:build !windows

package archive

import "os"

// renameNoReplace renames src to dst on one volume and fails when dst exists (fs.ErrExist): a hard
// link to dst (link(2) never replaces), then the unlink of src. os.Rename is not used because it
// replaces an existing dst. Interrupted between the two, both names hold the one file; the next
// run sees the same file at both names and removes src (MoveReport stays exact, nothing is lost).
func renameNoReplace(src, dst string) error {
	if err := os.Link(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}

// isBusy: only Windows refuses a rename because another process holds the file.
func isBusy(error) bool { return false }

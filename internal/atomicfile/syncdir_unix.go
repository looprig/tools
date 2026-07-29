//go:build !windows

package atomicfile

import "os"

// syncDir durably syncs a directory's entries by opening it and calling
// Sync, so a preceding rename's directory-entry update survives a crash.
// POSIX filesystems require an explicit fsync of the containing directory
// for a rename to be crash-durable; syncing the renamed file itself is not
// sufficient.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

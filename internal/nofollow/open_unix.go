//go:build !windows

package nofollow

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// Open opens path with flag|O_NOFOLLOW so a symlink at the final path
// component is refused rather than traversed. A refusal (ELOOP, or EMLINK
// on platforms that report it that way) is reported as an error wrapping
// ErrSymlinkNotAllowed; every other error — including a definitive
// not-found — is returned exactly as os.OpenFile produced it, so callers
// can keep using os.IsNotExist/errors.Is(fs.ErrNotExist) against Open's
// result exactly as they would against a plain os.OpenFile call.
func Open(path string, flag int, perm os.FileMode) (*os.File, error) {
	file, err := os.OpenFile(path, flag|syscall.O_NOFOLLOW, perm)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EMLINK) {
			return nil, fmt.Errorf("%w: %w", ErrSymlinkNotAllowed, err)
		}
		return nil, err
	}
	return file, nil
}

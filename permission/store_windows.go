//go:build windows

package permission

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

// store_windows.go implements store.go's platform primitives for Windows:
// LockFileEx/UnlockFileEx for the interprocess lock, a
// FILE_FLAG_OPEN_REPARSE_POINT open for the O_NOFOLLOW symlink defense, and
// a SID/token owner comparison plus NumberOfLinks for the Stat_t-based
// owner/link-count identity checks. See store_unix.go for the POSIX
// counterparts and store.go for the shared logic that calls into these.
//
// None of this has been exercised against a real Windows filesystem or
// process token — it compiles and is a good-faith, invariant-preserving
// port, but real-machine validation belongs to the plan's Phase 5
// ("Unix PTY and Windows ConPTY") Windows-hardening pass. Faithfulness
// gaps against the Unix behavior are called out at each function below.

// openNoFollow opens path with the given flag/perm, refusing to traverse a
// reparse point (Windows's analogue of a symlink) at the final path
// component — the closest available equivalent of Unix's O_NOFOLLOW.
//
// Faithfulness gap: unlike O_NOFOLLOW, CreateFile has no flag that fails
// the open outright when the target is a reparse point.
// FILE_FLAG_OPEN_REPARSE_POINT instead succeeds and hands back a handle to
// the reparse point object itself (rather than transparently following it
// to its target, which is the unsafe behavior being defended against). This
// function closes that gap itself: it inspects the resulting handle's
// FILE_ATTRIBUTE_REPARSE_POINT bit via GetFileInformationByHandle and
// refuses (returning errSymlinkNotAllowed) before the handle is ever
// returned to a caller, so the net effect matches O_NOFOLLOW — traversal is
// refused, not silently allowed — even though the mechanism differs. One
// known imprecision: this refuses *any* reparse point (symlink, junction,
// mount point, or a third-party filesystem filter's reparse tag) rather
// than symlinks specifically; that is deliberately conservative (fail
// closed) rather than under-protective, but the Phase 5 pass should confirm
// it doesn't reject reparse points the store legitimately needs to open.
func openNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	var access uint32
	switch flag & (os.O_RDONLY | os.O_WRONLY | os.O_RDWR) {
	case os.O_WRONLY:
		access = windows.GENERIC_WRITE
	case os.O_RDWR:
		access = windows.GENERIC_READ | windows.GENERIC_WRITE
	default:
		access = windows.GENERIC_READ
	}
	if flag&os.O_CREATE != 0 {
		access |= windows.GENERIC_WRITE
	}
	createDisposition := uint32(windows.OPEN_EXISTING)
	if flag&os.O_CREATE != 0 {
		createDisposition = windows.OPEN_ALWAYS
	}
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	shareMode := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE)
	// FILE_FLAG_OPEN_REPARSE_POINT: open the reparse point object itself
	// instead of transparently following it to its target.
	attrs := uint32(windows.FILE_ATTRIBUTE_NORMAL | windows.FILE_FLAG_OPEN_REPARSE_POINT)
	handle, err := windows.CreateFile(pathPtr, access, shareMode, nil, createDisposition, attrs, 0)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil, fmt.Errorf("%w: %w", fs.ErrNotExist, err)
		}
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)

	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = file.Close()
		return nil, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = file.Close()
		return nil, fmt.Errorf("%w: refusing reparse point at %s", errSymlinkNotAllowed, path)
	}
	return file, nil
}

// lockExclusiveNonBlocking takes an exclusive, non-blocking byte-range lock
// on f via LockFileEx: LOCKFILE_EXCLUSIVE_LOCK requests exclusive
// (write) access and LOCKFILE_FAIL_IMMEDIATELY makes the call return at
// once rather than block, mirroring flock(LOCK_EX|LOCK_NB)'s semantics. The
// lock covers the file's first byte only; that is the standard Windows
// idiom for emulating a whole-file advisory lock (matching, e.g., the
// approach used by cmd/go's internal lockedfile package and the widely used
// gofrs/flock package) — all lockers in this codebase go through this same
// function, so the convention is self-consistent.
func lockExclusiveNonBlocking(f *os.File) error {
	overlapped := new(windows.Overlapped)
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped)
	if err == nil {
		return nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return errLockWouldBlock
	}
	return err
}

// unlockFile releases a lock taken by lockExclusiveNonBlocking. Callers
// treat it as best-effort: closing the handle also drops the lock even if
// the explicit unlock fails.
func unlockFile(f *os.File) error {
	overlapped := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, overlapped)
}

// checkFileIdentity enforces the owner and hard-link-count hardening
// checks for the permission file using the open handle: NumberOfLinks is a
// direct, faithful analogue of Stat_t.Nlink (NTFS hard links are real, and
// BY_HANDLE_FILE_INFORMATION reports the true link count). info is unused
// on this platform — Go's os.FileInfo.Sys() on Windows does not expose
// link count or owner, so both checks go through the handle instead.
func (s *Store) checkFileIdentity(file *os.File, info fs.FileInfo) error {
	var winInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &winInfo); err != nil {
		return &FileError{Path: s.path, Reason: FileIO, Err: err}
	}
	if winInfo.NumberOfLinks != 1 {
		return &FileError{Path: s.path, Reason: FileLinkCount, Err: fmt.Errorf("link count %d, require 1", winInfo.NumberOfLinks)}
	}
	return s.checkOwnerHandle(windows.Handle(file.Fd()), "owner")
}

// checkDirIdentity enforces the owner hardening check for the store
// directory, looked up by path (matching the os.Lstat-based check on Unix,
// which also has no open handle).
func (s *Store) checkDirIdentity(directory string, info fs.FileInfo) error {
	return s.checkOwnerPath(directory, "store directory owner")
}

// checkOwnerHandle and checkOwnerPath both resolve to the same SID
// comparison; they differ only in how the security descriptor is fetched
// (by open handle vs. by path), matching the two call sites' available
// state.
func (s *Store) checkOwnerHandle(handle windows.Handle, label string) error {
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return &FileError{Path: s.path, Reason: FileIO, Err: err}
	}
	return s.compareOwnerSID(sd, label)
}

func (s *Store) checkOwnerPath(path, label string) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return &FileError{Path: s.path, Reason: FileIO, Err: err}
	}
	return s.compareOwnerSID(sd, label)
}

// compareOwnerSID is the Windows analogue of the Unix "stat.Uid != s.euid"
// check: Windows has no UID, so the equivalent identity comparison is
// between the object's owner SID (from its security descriptor) and the
// current process token's user SID.
//
// Faithfulness gap: Unix's euid is captured once at Store construction
// (os.Geteuid(), cached in s.euid) so a later privilege change on the
// process doesn't move the goalposts mid-comparison; this Windows check
// re-reads the current process token's user SID on every call instead,
// since os.Geteuid() always returns -1 on Windows and s.euid carries no
// meaningful value there. A process whose token user changes between
// checks would see different results than the cached-euid Unix behavior.
// This is expected to be effectively unobservable in practice (a process's
// primary token user does not normally change post-creation) but the
// Phase 5 pass should confirm that assumption holds for how this store is
// actually run on Windows.
func (s *Store) compareOwnerSID(sd *windows.SECURITY_DESCRIPTOR, label string) error {
	owner, _, err := sd.Owner()
	if err != nil {
		return &FileError{Path: s.path, Reason: FileIO, Err: err}
	}
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return &FileError{Path: s.path, Reason: FileIO, Err: err}
	}
	if !owner.Equals(tokenUser.User.Sid) {
		return &FileError{Path: s.path, Reason: FileOwnerUnexpected, Err: fmt.Errorf("%s SID %s, require %s", label, owner.String(), tokenUser.User.Sid.String())}
	}
	return nil
}

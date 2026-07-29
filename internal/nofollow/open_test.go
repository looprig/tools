package nofollow

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestOpen covers the primitive's two central guarantees on this host
// platform: a plain regular file opens normally, and a symlinked path is
// refused with an error wrapping ErrSymlinkNotAllowed rather than being
// silently followed.
func TestOpen(t *testing.T) {
	dir := t.TempDir()

	regular := filepath.Join(dir, "regular.txt")
	if err := os.WriteFile(regular, []byte("hello"), 0o600); err != nil {
		t.Fatalf("seed regular file: %v", err)
	}

	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(regular, link); err != nil {
		t.Skipf("symlink not supported on this host/platform: %v", err)
	}

	missing := filepath.Join(dir, "missing.txt")

	tests := []struct {
		name        string
		path        string
		flag        int
		wantErr     bool
		wantSymlink bool
		wantExist   bool
	}{
		{name: "regular file opens", path: regular, flag: os.O_RDONLY},
		{name: "symlinked path is refused", path: link, flag: os.O_RDONLY, wantErr: true, wantSymlink: true},
		{name: "missing path reports not-exist", path: missing, flag: os.O_RDONLY, wantErr: true, wantExist: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := Open(tt.path, tt.flag, 0)
			if tt.wantErr {
				if err == nil {
					_ = f.Close()
					t.Fatalf("Open(%q): expected an error, got none", tt.path)
				}
				if tt.wantSymlink && !errors.Is(err, ErrSymlinkNotAllowed) {
					t.Errorf("Open(%q): error %v does not wrap ErrSymlinkNotAllowed", tt.path, err)
				}
				if tt.wantExist && !errors.Is(err, os.ErrNotExist) {
					t.Errorf("Open(%q): error %v does not wrap os.ErrNotExist", tt.path, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Open(%q): unexpected error: %v", tt.path, err)
			}
			defer func() { _ = f.Close() }()
			data := make([]byte, 5)
			n, err := f.Read(data)
			if err != nil || n != 5 || string(data) != "hello" {
				t.Errorf("Open(%q): read back %q (n=%d, err=%v), want %q", tt.path, data[:n], n, err, "hello")
			}
		})
	}
}

// TestOpenCreateExcl covers the O_CREATE|O_EXCL no-clobber case
// filemutation's stageTempFile relies on: the first create succeeds and the
// second (of the same path) fails because the file now exists.
func TestOpenCreateExcl(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "created.txt")

	f, err := Open(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("first create: unexpected error: %v", err)
	}
	_ = f.Close()

	if _, err := Open(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err == nil {
		t.Fatalf("second create over an existing file: expected an error, got none")
	} else if errors.Is(err, ErrSymlinkNotAllowed) {
		t.Errorf("second create: got a symlink refusal, want an already-exists error: %v", err)
	}
}

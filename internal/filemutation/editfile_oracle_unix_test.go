//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package filemutation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestPrepareCallNeverReadsTheTargetContent observes target opens directly.
// A FIFO reader blocks until a writer appears, letting the test distinguish
// path metadata resolution (permitted during preparation) from opening the
// target for content (forbidden before the gate). The calibration first proves
// the observer detects a real os.ReadFile on this platform/filesystem.
func TestPrepareCallNeverReadsTheTargetContent(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "a.go")
	if err := unix.Mkfifo(target, 0o600); err != nil {
		t.Skipf("FIFO unsupported: %v", err)
	}

	opened, err := targetOpenedDuring(t, target, func() error {
		_, err := os.ReadFile(target)
		return err
	})
	if err != nil {
		t.Fatalf("calibration read failed: %v", err)
	}
	if !opened {
		t.Fatal("FIFO observer did not detect a real target read")
	}

	edit := NewEditFile(root, newFileObservations())
	executionID := mustUUID(t)
	argsJSON := mustJSON(t, map[string]any{"path": "a.go", "old": "anything", "new": "new"})
	opened, err = targetOpenedDuring(t, target, func() error {
		_, _, err := edit.PrepareCall(
			context.Background(),
			executionID,
			argsJSON,
		)
		return err
	})
	if err != nil {
		t.Fatalf("PrepareCall failed, leaking file state to the model: %v", err)
	}
	if opened {
		t.Fatal("PrepareCall opened the target before permission gating")
	}
}

// targetOpenedDuring runs action while probing fifo for a blocked reader. A
// nonblocking writer can open a FIFO only while a reader is present; opening
// and closing that writer also releases a forbidden read so the test cannot
// strand a goroutine when it detects the regression.
func targetOpenedDuring(t *testing.T, fifo string, action func() error) (bool, error) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- action()
	}()

	probe := time.NewTicker(time.Millisecond)
	defer probe.Stop()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()

	for {
		select {
		case err := <-done:
			return false, err
		case <-probe.C:
			fd, err := unix.Open(fifo, unix.O_WRONLY|unix.O_NONBLOCK, 0)
			if err == nil {
				if closeErr := unix.Close(fd); closeErr != nil {
					return true, fmt.Errorf("close FIFO probe writer: %w", closeErr)
				}
				select {
				case actionErr := <-done:
					return true, actionErr
				case <-time.After(5 * time.Second):
					return true, errors.New("target reader remained blocked after FIFO writer closed")
				}
			}
			if !errors.Is(err, unix.ENXIO) {
				return false, fmt.Errorf("probe FIFO writer: %w", err)
			}
		case <-timeout.C:
			return false, errors.New("timed out waiting for preparation or a target open")
		}
	}
}

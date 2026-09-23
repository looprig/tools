//go:build !windows

package bash

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// maxReturnAfterTimeout bounds how long a timed-out or finished command may
// keep the call open because a descendant still holds its output pipe.
const maxReturnAfterTimeout = 5 * time.Second

// TestBashTimeoutReturnsPromptlyWithAnOrphanedDescendant proves a timed-out
// command returns near its timeout even when descendants of sh (a background
// job, and a foreground child sh has not exec'd into) still hold the output
// pipe. Before the fix the call lasted as long as the longest descendant.
func TestBashTimeoutReturnsPromptlyWithAnOrphanedDescendant(t *testing.T) {
	t.Parallel()
	requireSh(t)
	for name, sink := range map[string]*recordingSink{"uncaptured": nil, "captured": {}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			raw, _ := json.Marshal(map[string]any{"command": "sleep 30 & printf x; sleep 30; true", "timeout": 1})
			start := time.Now()
			var got string
			if sink == nil {
				got = runPrepared(t, NewBash(t.TempDir()), string(raw), nil)
			} else {
				got = runCaptured(t, context.Background(), NewBash(t.TempDir()), string(raw), sink)
			}
			if elapsed := time.Since(start); elapsed > maxReturnAfterTimeout {
				t.Fatalf("timed-out call returned after %v, want <= %v", elapsed, maxReturnAfterTimeout)
			}
			if got != "error: command timed out after 1s" {
				t.Fatalf("result = %q, want the timeout error", got)
			}
			if sink != nil && sink.String() != "x\nerror: command timed out after 1s" {
				t.Fatalf("captured = %q", sink.String())
			}
		})
	}
}

// TestBashExitReturnsPromptlyWhenABackgroundJobHoldsThePipe proves a command
// that exits normally while a background job it started still holds the
// output pipe returns its own exit status promptly, rather than waiting for
// the job (or being misreported as a start failure).
func TestBashExitReturnsPromptlyWhenABackgroundJobHoldsThePipe(t *testing.T) {
	t.Parallel()
	requireSh(t)
	// os/exec reports a failing exit as *ExitError and a successful one as
	// ErrWaitDelay, so both exit statuses are covered.
	for _, code := range []string{"0", "3"} {
		t.Run("exit "+code, func(t *testing.T) {
			t.Parallel()
			start := time.Now()
			got := runPrepared(t, NewBash(t.TempDir()), commandArgs("sleep 30 & printf done; exit "+code), nil)
			if elapsed := time.Since(start); elapsed > maxReturnAfterTimeout {
				t.Fatalf("call returned after %v, want <= %v", elapsed, maxReturnAfterTimeout)
			}
			if want := "done\n[exit code: " + code + "]"; got != want {
				t.Fatalf("result = %q, want %q", got, want)
			}
		})
	}
}

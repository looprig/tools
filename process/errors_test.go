package process

import (
	"errors"
	"strings"
	"testing"
)

// allDefinedCodes lists every stable error code from the spec's "Stable
// errors" section, authored independently of the production `codes` map, so
// this test exercises the full closed taxonomy rather than re-deriving it
// from the implementation.
var allDefinedCodes = []Code{
	CodeInvalidArguments,
	CodeInvalidSettings,
	CodeProcessQuotaExceeded,
	CodeOutputQuotaExceeded,
	CodeLifetimeEnforcementUnavailable,
	CodeProcessNotificationsUnsupported,
	CodeSpawnFailed,
	CodeProcessSetupFailed,
	CodePTYUnavailable,
	CodeNotFound,
	CodeStdinClosed,
	CodeInputBackpressure,
	CodeCursorGap,
	CodeCursorAhead,
	CodeTimedOut,
	CodeInterrupted,
	CodeTerminated,
	CodeKilled,
	CodeSupervisorShuttingDown,
	CodeManifestCorrupt,
	CodeSpoolCorrupt,
	CodeLostOnRestore,
	CodeTeardownFailed,
}

// wantCodeStrings pins the exact wire string for every code, independent of
// the production const declarations, so a typo in errors.go is caught.
var wantCodeStrings = map[Code]string{
	CodeInvalidArguments:                "invalid_arguments",
	CodeInvalidSettings:                 "invalid_settings",
	CodeProcessQuotaExceeded:            "process_quota_exceeded",
	CodeOutputQuotaExceeded:             "output_quota_exceeded",
	CodeLifetimeEnforcementUnavailable:  "lifetime_enforcement_unavailable",
	CodeProcessNotificationsUnsupported: "process_notifications_unsupported",
	CodeSpawnFailed:                     "spawn_failed",
	CodeProcessSetupFailed:              "process_setup_failed",
	CodePTYUnavailable:                  "pty_unavailable",
	CodeNotFound:                        "not_found",
	CodeStdinClosed:                     "stdin_closed",
	CodeInputBackpressure:               "input_backpressure",
	CodeCursorGap:                       "cursor_gap",
	CodeCursorAhead:                     "cursor_ahead",
	CodeTimedOut:                        "timed_out",
	CodeInterrupted:                     "interrupted",
	CodeTerminated:                      "terminated",
	CodeKilled:                          "killed",
	CodeSupervisorShuttingDown:          "supervisor_shutting_down",
	CodeManifestCorrupt:                 "manifest_corrupt",
	CodeSpoolCorrupt:                    "spool_corrupt",
	CodeLostOnRestore:                   "lost_on_restore",
	CodeTeardownFailed:                  "teardown_failed",
}

func TestCodeWireValue(t *testing.T) {
	t.Parallel()
	if len(allDefinedCodes) != len(wantCodeStrings) {
		t.Fatalf("test fixture bug: %d codes listed, %d wire strings pinned", len(allDefinedCodes), len(wantCodeStrings))
	}
	for _, c := range allDefinedCodes {
		want, ok := wantCodeStrings[c]
		if !ok {
			t.Fatalf("test fixture bug: no pinned wire string for %v", c)
		}
		if string(c) != want {
			t.Errorf("Code value = %q, want %q", string(c), want)
		}
	}
}

func TestCodeValid(t *testing.T) {
	t.Parallel()
	for _, c := range allDefinedCodes {
		if !c.Valid() {
			t.Errorf("Code(%q).Valid() = false, want true", c)
		}
	}
	for _, bad := range []Code{"", "bogus", "Invalid_Arguments", "not_found "} {
		if bad.Valid() {
			t.Errorf("Code(%q).Valid() = true, want false", bad)
		}
	}
}

// TestErrorCodeRoundTrip verifies constructing an *Error from a Code and
// reading it back never loses or mutates the code, for every code in the
// closed taxonomy.
func TestErrorCodeRoundTrip(t *testing.T) {
	t.Parallel()
	for _, c := range allDefinedCodes {
		err := New(c)
		if err.Code != c {
			t.Errorf("New(%v).Code = %v, want %v", c, err.Code, c)
		}
		if !errors.Is(err, New(c)) {
			t.Errorf("errors.Is(New(%v), New(%v)) = false, want true", c, c)
		}
	}
}

// TestErrorIsMatchesByCodeOnly verifies errors.Is distinguishes *Error
// values purely by Code, regardless of Cause, and that unrelated codes never
// match.
func TestErrorIsMatchesByCodeOnly(t *testing.T) {
	t.Parallel()
	cause := errors.New("underlying detail")

	bare := New(CodeNotFound)
	wrapped := Wrap(CodeNotFound, cause)
	if !errors.Is(wrapped, bare) {
		t.Errorf("errors.Is(wrapped, bare) = false, want true: Is must match by Code alone, ignoring Cause")
	}
	if !errors.Is(bare, wrapped) {
		t.Errorf("errors.Is(bare, wrapped) = false, want true (symmetric)")
	}

	other := New(CodeTimedOut)
	if errors.Is(bare, other) {
		t.Errorf("errors.Is(%v, %v) = true, want false: different codes must not match", bare, other)
	}

	// A target that isn't a *Error must never match.
	if errors.Is(bare, cause) {
		t.Errorf("errors.Is(bare, cause) = true, want false: a non-*Error target must never match")
	}
}

// TestErrorUnwrapReturnsCause verifies Unwrap surfaces the wrapped cause so
// errors.Is/errors.As can see through an *Error to a sentinel or typed
// error underneath, and that a bare (causeless) Error unwraps to nil.
func TestErrorUnwrapReturnsCause(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("sentinel cause")
	wrapped := Wrap(CodeSpawnFailed, sentinel)
	if !errors.Is(wrapped, sentinel) {
		t.Errorf("errors.Is(wrapped, sentinel) = false, want true")
	}
	if got := wrapped.Unwrap(); got != sentinel {
		t.Errorf("Unwrap() = %v, want %v", got, sentinel)
	}

	bare := New(CodeSpawnFailed)
	if got := bare.Unwrap(); got != nil {
		t.Errorf("Unwrap() on causeless Error = %v, want nil", got)
	}
}

// TestErrorMessageContainsCodeAndCause verifies Error() renders the stable
// code and, when present, the cause's message.
func TestErrorMessageContainsCodeAndCause(t *testing.T) {
	t.Parallel()
	bare := New(CodeManifestCorrupt)
	if !strings.Contains(bare.Error(), string(CodeManifestCorrupt)) {
		t.Errorf("Error() = %q, want it to mention code %q", bare.Error(), CodeManifestCorrupt)
	}

	cause := errors.New("truncated header")
	wrapped := Wrap(CodeManifestCorrupt, cause)
	msg := wrapped.Error()
	if !strings.Contains(msg, string(CodeManifestCorrupt)) || !strings.Contains(msg, cause.Error()) {
		t.Errorf("Error() = %q, want it to mention both code %q and cause %q", msg, CodeManifestCorrupt, cause.Error())
	}
}

// TestErrorAsExtractsConcreteType verifies errors.As can recover the
// concrete *Error (and thus its Code) from a value typed as plain error.
func TestErrorAsExtractsConcreteType(t *testing.T) {
	t.Parallel()
	var err error = Wrap(CodeCursorGap, errors.New("cause"))
	var target *Error
	if !errors.As(err, &target) {
		t.Fatalf("errors.As() = false, want true")
	}
	if target.Code != CodeCursorGap {
		t.Errorf("extracted Code = %v, want %v", target.Code, CodeCursorGap)
	}
}

// TestNilErrorIsSafe verifies the defensive nil-receiver handling in Error,
// Unwrap, and Is (mirroring harness's ProcessError.Is nil-safety pattern).
func TestNilErrorIsSafe(t *testing.T) {
	t.Parallel()
	var nilErr *Error
	if got := nilErr.Error(); got != "<nil>" {
		t.Errorf("nil Error() = %q, want %q", got, "<nil>")
	}
	if got := nilErr.Unwrap(); got != nil {
		t.Errorf("nil Unwrap() = %v, want nil", got)
	}
	if nilErr.Is(New(CodeNotFound)) {
		t.Errorf("nil.Is(New(...)) = true, want false")
	}
}

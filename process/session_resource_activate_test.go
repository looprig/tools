package process

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/tool"
)

// session_resource_activate_test.go proves SupervisorResource.Activate
// actually wires a REAL, validated tool.SessionResourceServices -- the exact
// shape Harness's live session construction supplies -- into the shared
// Supervisor this resource wraps, so a real admitted process's Start-time
// and terminal lifecycle publications and its completion notification reach
// Harness's real tool.ProcessLifecyclePublisher/tool.ProcessCompletionNotifier
// as DTOs that pass Harness's OWN Validate(). Before this file's fix,
// Activate was a documented no-op (see session_resource.go's prior doc
// comment, which this test file drove out) and NewSupervisorResource always
// constructed the Supervisor with nil lifecycle/notifications, so this test
// failed with zero calls to either fake even though a real process ran to
// completion -- entirely undetected by every existing package-private-fake
// test (entry_test.go/supervisor_test.go/supervisor_integration_test.go),
// none of which ever exercises the real harness tool.SessionResourceServices
// seam or validates the resulting DTO shape.

// fakeToolLifecyclePublisher and fakeToolCompletionNotifier are harness-facing
// (tool.ProcessLifecyclePublisher/tool.ProcessCompletionNotifier) fakes --
// distinct from entry_test.go's package-private fakeLifecycleSink/
// fakeCompletionNotifier, which satisfy this package's OWN narrower
// lifecycleSink/completionNotifier interfaces and therefore never touch a
// real tool.ProcessLifecycleMetadata/tool.ProcessCompletionNotification DTO
// at all. These exist to prove the seam a real Coderig/Harness session
// actually uses end to end.
type fakeToolLifecyclePublisher struct {
	mu    sync.Mutex
	calls []tool.ProcessLifecycleMetadata
}

func (f *fakeToolLifecyclePublisher) PublishProcessLifecycle(_ context.Context, m tool.ProcessLifecycleMetadata) error {
	f.mu.Lock()
	f.calls = append(f.calls, m)
	f.mu.Unlock()
	return nil
}

func (f *fakeToolLifecyclePublisher) Calls() []tool.ProcessLifecycleMetadata {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]tool.ProcessLifecycleMetadata(nil), f.calls...)
}

var _ tool.ProcessLifecyclePublisher = (*fakeToolLifecyclePublisher)(nil)

type fakeToolCompletionNotifier struct {
	mu    sync.Mutex
	calls []tool.ProcessCompletionNotification
}

func (f *fakeToolCompletionNotifier) NotifyProcessCompletion(_ context.Context, n tool.ProcessCompletionNotification) error {
	f.mu.Lock()
	f.calls = append(f.calls, n)
	f.mu.Unlock()
	return nil
}

func (f *fakeToolCompletionNotifier) Calls() []tool.ProcessCompletionNotification {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]tool.ProcessCompletionNotification(nil), f.calls...)
}

var _ tool.ProcessCompletionNotifier = (*fakeToolCompletionNotifier)(nil)

// TestSupervisorResourceActivateWiresRealLifecycleAndNotificationServices is
// this file's headline proof. See the file doc comment above.
func TestSupervisorResourceActivateWiresRealLifecycleAndNotificationServices(t *testing.T) {
	resource, err := NewSupervisorResource(t.TempDir())
	if err != nil {
		t.Fatalf("NewSupervisorResource() err = %v, want nil", err)
	}
	sr, ok := resource.(*SupervisorResource)
	if !ok || sr == nil || sr.Supervisor == nil {
		t.Fatalf("NewSupervisorResource() = %T, want *SupervisorResource with a live Supervisor", resource)
	}
	t.Cleanup(func() { _ = sr.Shutdown(context.Background()) })

	publisher := &fakeToolLifecyclePublisher{}
	notifier := &fakeToolCompletionNotifier{}
	services, err := tool.NewSessionResourceServices(publisher, notifier)
	if err != nil {
		t.Fatalf("tool.NewSessionResourceServices() err = %v, want nil", err)
	}
	if err := resource.Activate(context.Background(), services); err != nil {
		t.Fatalf("Activate() err = %v, want nil", err)
	}

	owner := testOwner(t)
	origin := testOrigin(t)
	proc := &fakeProcess{waitResult: tool.ProcessResult{ExitCode: 0, Reason: tool.ProcessTerminalExited, FinishedAt: time.Now()}}
	prepared := &fakePreparedProcess{process: proc}
	lease := &fakeLease{}

	// sink (the 6th positional Start argument) is deliberately nil here,
	// exactly mirroring bash/supervised.go's real production call
	// (runSupervised passes nil for both sink and observations) -- the
	// whole point of this test is that a REAL caller never supplies a
	// per-call sink, and Activate's session-wide wiring must still reach
	// the terminal publish/notify without one.
	handle, err := sr.Supervisor.Start(context.Background(), owner, origin, prepared, lease, nil, nil, StorageCeiling{}, YieldSettings{})
	if err != nil {
		t.Fatalf("Start() err = %v, want nil", err)
	}

	e := testEntry(t, sr.Supervisor, handle)
	select {
	case <-e.done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the entry to terminalize")
	}

	published := publisher.Calls()
	if len(published) != 2 {
		t.Fatalf("PublishProcessLifecycle calls = %d, want 2 (Started, then Completed)", len(published))
	}
	if published[0].Kind != tool.ProcessLifecycleStarted {
		t.Errorf("published[0].Kind = %v, want Started", published[0].Kind)
	}
	if published[1].Kind != tool.ProcessLifecycleCompleted {
		t.Errorf("published[1].Kind = %v, want Completed", published[1].Kind)
	}
	for i, m := range published {
		if err := m.Validate(); err != nil {
			t.Errorf("published[%d] = %+v failed tool.ProcessLifecycleMetadata.Validate(): %v", i, m, err)
		}
		if m.ProcessHandle != string(handle) {
			t.Errorf("published[%d].ProcessHandle = %q, want %q", i, m.ProcessHandle, handle)
		}
		if m.SessionID != owner.SessionID || m.LoopID != owner.LoopID {
			t.Errorf("published[%d] owner = %s/%s, want %s/%s", i, m.SessionID, m.LoopID, owner.SessionID, owner.LoopID)
		}
	}
	if published[1].State != tool.ProcessLifecycleExited || !published[1].HasExitCode || published[1].ExitCode != 0 {
		t.Errorf("published[1] terminal fields = %+v, want exited/HasExitCode/exit 0", published[1])
	}

	notified := notifier.Calls()
	if len(notified) != 1 {
		t.Fatalf("NotifyProcessCompletion calls = %d, want 1", len(notified))
	}
	if err := notified[0].Validate(); err != nil {
		t.Errorf("notification %+v failed tool.ProcessCompletionNotification.Validate(): %v", notified[0], err)
	}
	if notified[0].ProcessHandle != string(handle) || notified[0].State != tool.ProcessLifecycleExited {
		t.Errorf("notification = %+v, want handle %q state Exited", notified[0], handle)
	}
	if notified[0].CommandID.IsZero() {
		t.Error("notification CommandID is zero, want the manifest's pre-persisted stable CommandID")
	}
}

// TestSupervisorResourceActivateRejectsInvalidServices proves Activate fails
// closed on an unvalidated/invalid service set rather than silently leaving
// the Supervisor half-wired or panicking.
func TestSupervisorResourceActivateRejectsInvalidServices(t *testing.T) {
	resource, err := NewSupervisorResource(t.TempDir())
	if err != nil {
		t.Fatalf("NewSupervisorResource() err = %v, want nil", err)
	}
	t.Cleanup(func() {
		sr := resource.(*SupervisorResource)
		_ = sr.Shutdown(context.Background())
	})

	if err := resource.Activate(context.Background(), tool.SessionResourceServices{}); err == nil {
		t.Fatal("Activate(zero services) err = nil, want a validation error")
	}
}

package process

// definitions_test.go proves, from inside package process, the exact
// construction recipe Task 19's root tools.Definition builders (module root
// definitions.go) depend on: NewManifestStore + NewSupervisor(Config{},
// manifests, dir, nil, nil) builds a runner-free *Supervisor (it constructs
// no tool.AsyncProcessRunner and calls neither PrepareProcess nor Start)
// that NewProcessOutput, NewProcessInput, and NewProcessStop can all share.
// This is the process-package half of "any of the four [process-backed]
// definitions may win the get-or-create race" (bash/supervised.go's own doc
// comment; spec "Workspace coordination": "Bash, ProcessOutput,
// ProcessInput, and ProcessStop can each win the registry's get-or-create
// race because the supervisor contains no runner").

import (
	"context"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
)

// newRunnerFreeTestSupervisor builds a *Supervisor exactly the way the
// exported NewSupervisorResource factory (session_resource.go) does — the
// same factory bash/supervised.go and this module's root definitions.go both
// obtain through tool.SessionResourceRegistry.GetOrCreate: from only a
// reserved storage directory, with no lifecycle publisher, no completion
// notifier, and no tool.AsyncProcessRunner anywhere in the call.
func newRunnerFreeTestSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	dir := t.TempDir()
	manifests := NewManifestStore(dir)
	supervisor, err := NewSupervisor(Config{}, manifests, dir, nil, nil)
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	return supervisor
}

// TestProcessDefinitionConstructorsShareOneRunnerFreeSupervisor proves the
// three companion constructors all accept and correctly bind to ONE shared
// Supervisor built with no runner involved anywhere in its construction —
// the exact shape a root Definition's Build closure hands them after
// resolving the shared supervisor session resource through the harness
// registry.
func TestProcessDefinitionConstructorsShareOneRunnerFreeSupervisor(t *testing.T) {
	t.Parallel()
	supervisor := newRunnerFreeTestSupervisor(t)
	owner := Owner{
		SessionID: uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		LoopID:    uuid.MustParse("22222222-2222-4222-8222-222222222222"),
	}

	tests := []struct {
		name     string
		built    tool.InvokableTool
		wantName string
	}{
		{name: "output", built: NewProcessOutput(supervisor, owner), wantName: "ProcessOutput"},
		{name: "input", built: NewProcessInput(supervisor, owner), wantName: "ProcessInput"},
		{name: "stop", built: NewProcessStop(supervisor, owner), wantName: "ProcessStop"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			info, err := test.built.Info(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if info.Name != test.wantName {
				t.Fatalf("Info().Name = %q, want %q", info.Name, test.wantName)
			}
			if _, ok := test.built.(tool.CallPreparer); !ok {
				t.Fatalf("%T does not implement tool.CallPreparer", test.built)
			}
		})
	}
}

// TestProcessDefinitionConstructorsFailClosedWithoutSupervisor proves every
// companion constructor fails closed (a non-nil PrepareCall error, never a
// panic) when handed no supervisor — the shape a root Definition's Build
// closure must reject BEFORE ever constructing one of these tools if it
// cannot resolve the shared supervisor session resource.
func TestProcessDefinitionConstructorsFailClosedWithoutSupervisor(t *testing.T) {
	t.Parallel()
	owner := Owner{
		SessionID: uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		LoopID:    uuid.MustParse("22222222-2222-4222-8222-222222222222"),
	}
	tests := []struct {
		name    string
		prepare tool.CallPreparer
	}{
		{name: "output", prepare: NewProcessOutput(nil, owner)},
		{name: "input", prepare: NewProcessInput(nil, owner)},
		{name: "stop", prepare: NewProcessStop(nil, owner)},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			executionID := uuid.MustParse("33333333-3333-4333-8333-333333333333")
			if _, _, err := test.prepare.PrepareCall(context.Background(), executionID, "{}"); err == nil {
				t.Fatal("PrepareCall() error = nil, want non-nil for a supervisor-less companion tool")
			}
		})
	}
}

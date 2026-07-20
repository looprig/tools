package todo

import (
	"context"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
)

// TestTodoPrepareCallIsPure pins Todo as a PURE prepared tool: PrepareCall
// returns an empty request (no requirements — no external effect to gate) so
// the runner does not fail it closed as an unprepared effectful tool.
func TestTodoPrepareCallIsPure(t *testing.T) {
	t.Parallel()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	req, art, err := NewTodo().PrepareCall(context.Background(), id, `{"action":"list"}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	if err := tool.ValidateRequest(req); err != nil {
		t.Fatalf("ValidateRequest() error = %v", err)
	}
	if len(req.Requirements) != 0 {
		t.Errorf("Requirements = %+v, want none (pure tool)", req.Requirements)
	}
	if art != nil {
		t.Errorf("artifact = %v, want nil (pure tool)", art)
	}
}

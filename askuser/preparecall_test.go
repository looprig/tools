package askuser

import (
	"context"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
)

// TestAskUserPrepareCallIsPure pins AskUser as a PURE prepared tool: it routes
// a question through the loop's own user-input capability (no external
// effect), so PrepareCall returns an empty request and the runner does not
// fail it closed as an unprepared effectful tool.
func TestAskUserPrepareCallIsPure(t *testing.T) {
	t.Parallel()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	req, art, err := NewAskUser().PrepareCall(context.Background(), id, `{"question":"q?"}`)
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

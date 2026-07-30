package bash

import (
	"encoding/json"
	"testing"
	"time"
)

// TestRenderSupervisedResultLiveShape asserts liveSupervisedResult renders a
// "running"/backgrounded, process_id/next_cursor/output/started_at-carrying
// shape with no status-terminal fields (exit_code/reason/finished_at) or
// error.
func TestRenderSupervisedResultLiveShape(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	res := liveSupervisedResult("proc-1", 0, "", startedAt)
	text := textOf(t, res)
	var out supervisedResult
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", text, err)
	}
	if out.ProcessID != "proc-1" {
		t.Errorf("ProcessID = %q, want %q", out.ProcessID, "proc-1")
	}
	if out.Status != "running" {
		t.Errorf("Status = %q, want %q", out.Status, "running")
	}
	if !out.Backgrounded {
		t.Errorf("Backgrounded = false, want true")
	}
	if out.StartedAt != startedAt.Format(time.RFC3339Nano) {
		t.Errorf("StartedAt = %q, want %q", out.StartedAt, startedAt.Format(time.RFC3339Nano))
	}
	if out.ExitCode != nil || out.Reason != "" || out.FinishedAt != "" || out.DurationMS != nil || out.Error != "" {
		t.Errorf("got %+v, want no exit_code/reason/finished_at/duration_ms/error on a live result", out)
	}
}

// TestRenderSupervisedResultTerminalShape asserts terminalSupervisedResult
// renders a non-backgrounded shape carrying the given status/exit_code/
// reason plus a computed duration_ms.
func TestRenderSupervisedResultTerminalShape(t *testing.T) {
	t.Parallel()
	code := 3
	startedAt := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(250 * time.Millisecond)
	res := terminalSupervisedResult("exited", &code, "exited", startedAt, finishedAt, "hello\n")
	text := textOf(t, res)
	var out supervisedResult
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", text, err)
	}
	if out.Backgrounded {
		t.Errorf("Backgrounded = true, want false for a terminal result")
	}
	if out.ProcessID != "" {
		t.Errorf("ProcessID = %q, want empty (a terminal result names no live process)", out.ProcessID)
	}
	if out.Status != "exited" {
		t.Errorf("Status = %q, want %q", out.Status, "exited")
	}
	if out.ExitCode == nil || *out.ExitCode != 3 {
		t.Errorf("ExitCode = %v, want 3", out.ExitCode)
	}
	if out.Reason != "exited" {
		t.Errorf("Reason = %q, want %q", out.Reason, "exited")
	}
	if out.DurationMS == nil || *out.DurationMS != 250 {
		t.Errorf("DurationMS = %v, want 250", out.DurationMS)
	}
	if out.Output != "hello\n" {
		t.Errorf("Output = %q, want %q", out.Output, "hello\n")
	}
	if out.NextCursor != 0 {
		t.Errorf("NextCursor = %d, want 0 (the spec's terminal shape has no cursor/gap/truncation field)", out.NextCursor)
	}
}

// TestTerminalSupervisedResultOmitsDurationWithoutBothTimestamps asserts
// duration_ms is only ever computed when both started_at and finished_at
// are known — never a fabricated or partial value.
func TestTerminalSupervisedResultOmitsDurationWithoutBothTimestamps(t *testing.T) {
	t.Parallel()
	res := terminalSupervisedResult("failed", nil, "failed", time.Time{}, time.Now(), "")
	text := textOf(t, res)
	var out supervisedResult
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", text, err)
	}
	if out.DurationMS != nil {
		t.Errorf("DurationMS = %v, want nil without a started_at", out.DurationMS)
	}
	if out.StartedAt != "" {
		t.Errorf("StartedAt = %q, want empty for a zero Time", out.StartedAt)
	}
}

// TestSupervisedErrorResultOnlyPopulatesError asserts an error result's ONLY
// populated top-level JSON key is "error" — no process_id, status, or other
// field leaks alongside a failure.
func TestSupervisedErrorResultOnlyPopulatesError(t *testing.T) {
	t.Parallel()
	res := supervisedErrorResult("lifetime_enforcement_unavailable")
	text := textOf(t, res)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", text, err)
	}
	if len(raw) != 1 {
		t.Fatalf("result = %v, want exactly 1 populated field", raw)
	}
	value, ok := raw["error"]
	if !ok {
		t.Fatalf("result = %v, want an %q field", raw, "error")
	}
	var code string
	if err := json.Unmarshal(value, &code); err != nil || code != "lifetime_enforcement_unavailable" {
		t.Errorf("error = %q, want %q", value, "lifetime_enforcement_unavailable")
	}
}

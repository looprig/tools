package task

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
)

const wantTaskGetSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "taskId": {"type": "string", "description": "Task UUID to retrieve."}
  },
  "required": ["taskId"]
}`

func TestTaskGetInfoExactContract(t *testing.T) {
	taskTool := newTaskGet(newStore(nil))
	info, err := taskTool.Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if info == nil {
		t.Fatal("Info() returned nil info")
	}
	if got, want := info.Name, "TaskGet"; got != want {
		t.Fatalf("Info().Name = %q, want %q", got, want)
	}
	if got, want := info.Desc, "Get one complete task from this Loop's private task graph."; got != want {
		t.Fatalf("Info().Desc = %q, want %q", got, want)
	}
	assertJSONEqual(t, string(info.Schema), wantTaskGetSchema)
}

func TestTaskGetPrepareCallReturnsPureCanonicalTypedArtifact(t *testing.T) {
	executionID := uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	wantID := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	taskTool := newTaskGet(newStore(nil))
	request, artifact, err := taskTool.PrepareCall(context.Background(), executionID, `{"taskId":"AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA"}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	if want := (tool.Request{ToolName: "TaskGet"}); !reflect.DeepEqual(request, want) {
		t.Fatalf("request = %#v, want %#v", request, want)
	}
	if len(request.Requirements) != 0 {
		t.Fatalf("pure request requirements = %#v, want none", request.Requirements)
	}
	if artifact == nil {
		t.Fatal("PrepareCall() returned nil artifact")
	}
	got, ok := artifact.(*taskGetArtifact)
	if !ok || got == nil {
		t.Fatalf("artifact = %T, want non-nil *taskGetArtifact", artifact)
	}
	if got.Token != executionID.String() {
		t.Fatalf("artifact token = %q, want %q", got.Token, executionID.String())
	}
	if got.taskID != wantID.String() {
		t.Fatalf("artifact taskID = %q, want canonical %q", got.taskID, wantID)
	}
}

func TestTaskGetPrepareRejectsInvalidArguments(t *testing.T) {
	overSized := `{"taskId":"` + strings.Repeat("x", maxTaskArgsBytes) + `"}`
	tests := []struct {
		name string
		raw  string
	}{
		{name: "missing taskId", raw: `{}`},
		{name: "empty taskId", raw: `{"taskId":""}`},
		{name: "whitespace taskId", raw: `{"taskId":"  "}`},
		{name: "invalid UUID", raw: `{"taskId":"not-a-uuid"}`},
		{name: "zero UUID", raw: `{"taskId":"00000000-0000-0000-0000-000000000000"}`},
		{name: "wrong type", raw: `{"taskId":7}`},
		{name: "null taskId", raw: `{"taskId":null}`},
		{name: "array taskId", raw: `{"taskId":[]}`},
		{name: "object taskId", raw: `{"taskId":{}}`},
		{name: "unknown field", raw: `{"taskId":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","extra":true}`},
		{name: "case variant field", raw: `{"TaskId":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}`},
		{name: "duplicate field", raw: `{"taskId":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","taskId":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"}`},
		{name: "malformed", raw: `{"taskId":`},
		{name: "trailing JSON", raw: `{"taskId":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"} {}`},
		{name: "null root", raw: `null`},
		{name: "array root", raw: `[]`},
		{name: "scalar root", raw: `"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"`},
		{name: "raw argument byte limit", raw: overSized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			taskTool := newTaskGet(newStore(nil))
			_, artifact, err := taskTool.PrepareCall(context.Background(), uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc"), tt.raw)
			if artifact != nil {
				t.Fatalf("invalid preparation returned artifact %T", artifact)
			}
			assertPrepareError(t, err)
		})
	}
}

func TestTaskGetExecutionReturnsCompleteTaskIncludingDerivedBlocks(t *testing.T) {
	dependencyID := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	dependentID := uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	store := newStore(sequenceIDs(dependencyID, dependentID))
	mustCreate(t, store, createInput{
		Subject:     "dependency subject",
		Description: "dependency description",
		Metadata:    json.RawMessage(`{"kind":"dependency"}`),
	})
	mustCreate(t, store, createInput{
		Subject:     "dependent subject",
		Description: "dependent description",
		BlockedBy:   []string{dependencyID.String()},
		Metadata:    json.RawMessage(`{"kind":"dependent"}`),
	})
	taskTool := newTaskGet(store)
	_, artifact, err := taskTool.PrepareCall(context.Background(), uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc"), `{"taskId":"AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA"}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	result, err := taskTool.InvokableRun(preparedContext(artifact), `{"taskId":"`+dependentID.String()+`"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v, want nil", err)
	}
	got := taskEnvelope(t, result)
	if got.ID != dependencyID.String() || got.Subject != "dependency subject" || got.Description != "dependency description" {
		t.Fatalf("got task = %#v, want complete dependency snapshot", got)
	}
	if got.Status != StatusPending {
		t.Fatalf("got status = %q, want %q", got.Status, StatusPending)
	}
	if !reflect.DeepEqual(got.Blocks, []string{dependentID.String()}) {
		t.Fatalf("got derived blocks = %#v, want %#v", got.Blocks, []string{dependentID.String()})
	}
	if got.BlockedBy != nil {
		t.Fatalf("dependency BlockedBy = %#v, want omitted", got.BlockedBy)
	}
	if string(got.Metadata) != `{"kind":"dependency"}` {
		t.Fatalf("got metadata = %s, want complete metadata", got.Metadata)
	}
}

func TestTaskGetExecutionUsesPreparedIDNotRawArguments(t *testing.T) {
	wantID := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	otherID := uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	store := newStore(sequenceIDs(wantID, otherID))
	mustCreate(t, store, validCreateInput("prepared task"))
	mustCreate(t, store, validCreateInput("raw task"))
	taskTool := newTaskGet(store)
	_, artifact, err := taskTool.PrepareCall(context.Background(), uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc"), `{"taskId":"`+strings.ToUpper(wantID.String())+`"}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	result, err := taskTool.InvokableRun(preparedContext(artifact), `{"taskId":"`+otherID.String()+`"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v, want nil", err)
	}
	got := taskEnvelope(t, result)
	if got.ID != wantID.String() || got.Subject != "prepared task" {
		t.Fatalf("got task = %#v, want prepared task", got)
	}
}

func TestTaskGetUnknownIDIsModelVisibleError(t *testing.T) {
	taskTool := newTaskGet(newStore(nil))
	_, artifact, err := taskTool.PrepareCall(context.Background(), uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"), `{"taskId":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	result, err := taskTool.InvokableRun(preparedContext(artifact), `{"taskId":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v, want nil", err)
	}
	text := resultText(t, result)
	if !strings.HasPrefix(text, "error:") {
		t.Fatalf("unknown ID result = %q, want model-visible error", text)
	}
	if strings.Contains(text, "subject") || strings.Contains(text, "metadata") {
		t.Fatalf("unknown ID result echoed task text or metadata: %q", text)
	}
}

func TestTaskGetRejectsMissingNilAndCrossToolArtifacts(t *testing.T) {
	store := newStore(nil)
	taskTool := newTaskGet(store)
	createTool := newTaskCreate(store)
	_, createArtifact, err := createTool.PrepareCall(context.Background(), uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"), `{"subject":"subject","description":"description"}`)
	if err != nil {
		t.Fatalf("TaskCreate PrepareCall() error = %v", err)
	}
	var nilArtifact *taskGetArtifact
	tests := []struct {
		name    string
		context context.Context
	}{
		{name: "missing", context: context.Background()},
		{name: "nil typed", context: preparedContext(nilArtifact)},
		{name: "cross tool", context: preparedContext(createArtifact)},
		{name: "wrong token artifact", context: preparedContext(tool.TokenArtifact{Token: "wrong"})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := taskTool.InvokableRun(tt.context, `{"taskId":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}`)
			if err != nil {
				t.Fatalf("InvokableRun() error = %v, want nil", err)
			}
			text := resultText(t, result)
			if !strings.HasPrefix(text, "error:") {
				t.Fatalf("artifact rejection result = %q, want model-visible error", text)
			}
		})
	}
}

func TestTaskGetSequentialAndAuditSummary(t *testing.T) {
	taskTool := newTaskGet(newStore(nil))
	if !taskTool.Sequential() {
		t.Fatal("Sequential() = false, want true")
	}
	if got, want := taskTool.AuditSummary(`{"taskId":"secret"}`), "TaskGet"; got != want {
		t.Fatalf("AuditSummary() = %q, want %q", got, want)
	}
	var _ tool.InvokableTool = taskTool
	var _ tool.CallPreparer = taskTool
	var _ tool.Auditable = taskTool
	var _ tool.Sequential = taskTool
}

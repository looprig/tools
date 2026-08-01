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

const wantTaskListSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {}
}`

func TestTaskListInfoExactContract(t *testing.T) {
	taskTool := newTaskList(newStore(nil))
	info, err := taskTool.Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if info == nil {
		t.Fatal("Info() returned nil info")
	}
	if got, want := info.Name, "TaskList"; got != want {
		t.Fatalf("Info().Name = %q, want %q", got, want)
	}
	if got, want := info.Desc, "List every task in this Loop's private task graph in deterministic order."; got != want {
		t.Fatalf("Info().Desc = %q, want %q", got, want)
	}
	assertJSONEqual(t, string(info.Schema), wantTaskListSchema)
}

func TestTaskListAcceptsOnlyEmptyObjectAndReturnsEmptyArray(t *testing.T) {
	taskTool := newTaskList(newStore(nil))
	executionID := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	request, artifact, err := taskTool.PrepareCall(context.Background(), executionID, `{}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	if want := (tool.Request{ToolName: "TaskList"}); !reflect.DeepEqual(request, want) {
		t.Fatalf("request = %#v, want %#v", request, want)
	}
	if len(request.Requirements) != 0 {
		t.Fatalf("pure request requirements = %#v, want none", request.Requirements)
	}
	listArtifact, ok := artifact.(*taskListArtifact)
	if !ok || listArtifact == nil {
		t.Fatalf("artifact = %T, want non-nil *taskListArtifact", artifact)
	}
	if got, want := listArtifact.Token, executionID.String(); got != want {
		t.Fatalf("artifact token = %q, want %q", got, want)
	}

	result, err := taskTool.InvokableRun(preparedContext(artifact), `{"unexpected":true}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v, want nil", err)
	}
	if got, want := resultText(t, result), `{"tasks":[]}`; got != want {
		t.Fatalf("empty list result = %q, want %q", got, want)
	}
	var envelope struct {
		Tasks []Task `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(resultText(t, result)), &envelope); err != nil {
		t.Fatalf("empty list result JSON = %v", err)
	}
	if envelope.Tasks == nil {
		t.Fatal("empty list returned nil tasks slice, want non-nil []")
	}
}

func TestTaskListPrepareRejectsEveryInvalidRootShape(t *testing.T) {
	overSized := strings.Repeat("x", maxTaskArgsBytes+1)
	tests := []struct {
		name string
		raw  string
	}{
		{name: "malformed", raw: `{`},
		{name: "trailing JSON", raw: `{} {}`},
		{name: "null root", raw: `null`},
		{name: "array root", raw: `[]`},
		{name: "scalar root", raw: `"{}"`},
		{name: "whitespace only", raw: " \t\n "},
		{name: "unknown field", raw: `{"taskId":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}`},
		{name: "case variant field", raw: `{"Properties":{}}`},
		{name: "wrong type unknown field", raw: `{"properties":[]}`},
		{name: "duplicate field", raw: `{"x":1,"x":2}`},
		{name: "raw argument byte limit", raw: overSized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			taskTool := newTaskList(newStore(nil))
			_, artifact, err := taskTool.PrepareCall(context.Background(), uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"), tt.raw)
			if artifact != nil {
				t.Fatalf("invalid preparation returned artifact %T", artifact)
			}
			assertPrepareError(t, err)
		})
	}
}

func TestTaskListExecutionIsDeterministicAndDerivesBlocks(t *testing.T) {
	dependencyID := uuid.MustParse("dddddddd-dddd-4ddd-8ddd-dddddddddddd")
	targetID := uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	dependentBID := uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	dependentAID := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	store := newStore(sequenceIDs(dependencyID, targetID, dependentBID, dependentAID))
	mustCreate(t, store, validCreateInput("dependency"))
	targetInput := validCreateInput("target")
	targetInput.BlockedBy = []string{dependencyID.String()}
	mustCreate(t, store, targetInput)
	dependentBInput := validCreateInput("dependent b")
	dependentBInput.BlockedBy = []string{targetID.String()}
	mustCreate(t, store, dependentBInput)
	dependentAInput := validCreateInput("dependent a")
	dependentAInput.BlockedBy = []string{targetID.String()}
	mustCreate(t, store, dependentAInput)

	taskTool := newTaskList(store)
	_, artifact, err := taskTool.PrepareCall(context.Background(), uuid.MustParse("eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"), `{}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	first, err := taskTool.InvokableRun(preparedContext(artifact), `null`)
	if err != nil {
		t.Fatalf("first InvokableRun() error = %v, want nil", err)
	}
	second, err := taskTool.InvokableRun(preparedContext(artifact), `{"taskId":"changed"}`)
	if err != nil {
		t.Fatalf("second InvokableRun() error = %v, want nil", err)
	}
	firstText := resultText(t, first)
	secondText := resultText(t, second)
	if firstText != secondText {
		t.Fatalf("list output changed between identical runs:\nfirst:  %s\nsecond: %s", firstText, secondText)
	}

	var envelope struct {
		Tasks []Task `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(firstText), &envelope); err != nil {
		t.Fatalf("list result JSON = %v", err)
	}
	if got, want := len(envelope.Tasks), 4; got != want {
		t.Fatalf("task count = %d, want %d", got, want)
	}
	wantIDs := []string{dependentAID.String(), dependentBID.String(), targetID.String(), dependencyID.String()}
	for index, wantID := range wantIDs {
		if got := envelope.Tasks[index].ID; got != wantID {
			t.Fatalf("task[%d].ID = %q, want %q", index, got, wantID)
		}
	}
	if got, want := envelope.Tasks[0].BlockedBy, []string{targetID.String()}; !reflect.DeepEqual(got, want) {
		t.Fatalf("dependent A BlockedBy = %#v, want %#v", got, want)
	}
	if got, want := envelope.Tasks[1].BlockedBy, []string{targetID.String()}; !reflect.DeepEqual(got, want) {
		t.Fatalf("dependent B BlockedBy = %#v, want %#v", got, want)
	}
	if got, want := envelope.Tasks[2].BlockedBy, []string{dependencyID.String()}; !reflect.DeepEqual(got, want) {
		t.Fatalf("target BlockedBy = %#v, want %#v", got, want)
	}
	if got, want := envelope.Tasks[2].Blocks, []string{dependentAID.String(), dependentBID.String()}; !reflect.DeepEqual(got, want) {
		t.Fatalf("target derived Blocks = %#v, want %#v", got, want)
	}
	if got, want := envelope.Tasks[3].Blocks, []string{targetID.String()}; !reflect.DeepEqual(got, want) {
		t.Fatalf("dependency derived Blocks = %#v, want %#v", got, want)
	}
}

func TestTaskListRejectsMissingNilAndCrossToolArtifacts(t *testing.T) {
	store := newStore(nil)
	taskTool := newTaskList(store)
	getTool := newTaskGet(store)
	_, getArtifact, err := getTool.PrepareCall(context.Background(), uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"), `{"taskId":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}`)
	if err != nil {
		t.Fatalf("TaskGet PrepareCall() error = %v", err)
	}
	var nilArtifact *taskListArtifact
	tests := []struct {
		name    string
		context context.Context
	}{
		{name: "missing", context: context.Background()},
		{name: "nil typed", context: preparedContext(nilArtifact)},
		{name: "cross tool", context: preparedContext(getArtifact)},
		{name: "wrong token artifact", context: preparedContext(tool.TokenArtifact{Token: "wrong"})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := taskTool.InvokableRun(tt.context, `{"taskId":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}`)
			if err != nil {
				t.Fatalf("InvokableRun() error = %v, want nil", err)
			}
			if text := resultText(t, result); !strings.HasPrefix(text, "error:") {
				t.Fatalf("artifact rejection result = %q, want model-visible error", text)
			}
		})
	}
}

func TestTaskListSequentialAndAuditSummary(t *testing.T) {
	taskTool := newTaskList(newStore(nil))
	if !taskTool.Sequential() {
		t.Fatal("Sequential() = false, want true")
	}
	if got, want := taskTool.AuditSummary(`{"taskId":"secret"}`), "TaskList"; got != want {
		t.Fatalf("AuditSummary() = %q, want %q", got, want)
	}
	var _ tool.InvokableTool = taskTool
	var _ tool.CallPreparer = taskTool
	var _ tool.Auditable = taskTool
	var _ tool.Sequential = taskTool
}

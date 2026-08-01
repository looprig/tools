package task

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
)

const wantTaskUpdateSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "taskId": {"type": "string", "description": "Task UUID to update."},
    "subject": {"type": "string", "description": "Replacement task title."},
    "description": {"type": "string", "description": "Replacement task requirements."},
    "activeForm": {"type": "string", "description": "Replacement present-continuous activity text."},
    "status": {"type": "string", "enum": ["pending", "in_progress", "completed", "deleted"], "description": "Replacement status, or deleted to remove the task."},
    "addBlockedBy": {"type": "array", "items": {"type": "string"}, "maxItems": 128, "description": "Existing task UUIDs to add as dependencies."},
    "removeBlockedBy": {"type": "array", "items": {"type": "string"}, "maxItems": 128, "description": "Task UUIDs to remove as dependencies."},
    "metadata": {"type": "object", "description": "Replacement metadata object; an empty object clears metadata."}
  },
  "required": ["taskId"]
}`

func TestTaskUpdateInfoExactContract(t *testing.T) {
	taskTool := newTaskUpdate(newStore(nil))
	info, err := taskTool.Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if info == nil {
		t.Fatal("Info() returned nil info")
	}
	if got, want := info.Name, "TaskUpdate"; got != want {
		t.Fatalf("Info().Name = %q, want %q", got, want)
	}
	if got, want := info.Desc, "Patch, transition, rewire, or delete one task in this Loop's private task graph."; got != want {
		t.Fatalf("Info().Desc = %q, want %q", got, want)
	}
	assertJSONEqual(t, string(info.Schema), wantTaskUpdateSchema)
}

func TestTaskUpdatePrepareCallReturnsPresenceAwareCanonicalArtifact(t *testing.T) {
	taskID := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	firstDependencyID := uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	secondDependencyID := uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	executionID := uuid.MustParse("dddddddd-dddd-4ddd-8ddd-dddddddddddd")
	taskTool := newTaskUpdate(newStore(nil))
	raw := `{"taskId":"AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA","subject":"replacement subject","description":"replacement description","activeForm":"Working on replacement","status":"deleted","addBlockedBy":["` + strings.ToUpper(secondDependencyID.String()) + `","` + firstDependencyID.String() + `","` + firstDependencyID.String() + `"],"removeBlockedBy":["` + strings.ToUpper(secondDependencyID.String()) + `"],"metadata":{"z":{"b":2,"a":1},"a":1}}`

	request, artifact, err := taskTool.PrepareCall(context.Background(), executionID, raw)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	if want := (tool.Request{ToolName: "TaskUpdate"}); !reflect.DeepEqual(request, want) {
		t.Fatalf("request = %#v, want %#v", request, want)
	}
	if len(request.Requirements) != 0 {
		t.Fatalf("pure request requirements = %#v, want none", request.Requirements)
	}
	updated, ok := artifact.(*taskUpdateArtifact)
	if !ok || updated == nil {
		t.Fatalf("artifact = %T, want non-nil *taskUpdateArtifact", artifact)
	}
	if got, want := updated.Token, executionID.String(); got != want {
		t.Fatalf("artifact token = %q, want %q", got, want)
	}
	if got, want := updated.input.TaskID, taskID.String(); got != want {
		t.Fatalf("artifact task ID = %q, want canonical %q", got, want)
	}
	if updated.input.Subject == nil || *updated.input.Subject != "replacement subject" {
		t.Fatalf("artifact subject = %#v, want present replacement", updated.input.Subject)
	}
	if updated.input.Description == nil || *updated.input.Description != "replacement description" {
		t.Fatalf("artifact description = %#v, want present replacement", updated.input.Description)
	}
	if updated.input.ActiveForm == nil || *updated.input.ActiveForm != "Working on replacement" {
		t.Fatalf("artifact active form = %#v, want present replacement", updated.input.ActiveForm)
	}
	if updated.input.Status == nil || *updated.input.Status != StatusCommandDeleted {
		t.Fatalf("artifact status = %#v, want deleted command", updated.input.Status)
	}
	if got, want := updated.input.AddBlockedBy, []string{firstDependencyID.String(), secondDependencyID.String()}; !reflect.DeepEqual(got, want) {
		t.Fatalf("artifact add dependencies = %#v, want %#v", got, want)
	}
	if got, want := updated.input.RemoveBlockedBy, []string{secondDependencyID.String()}; !reflect.DeepEqual(got, want) {
		t.Fatalf("artifact remove dependencies = %#v, want %#v", got, want)
	}
	if updated.input.Metadata == nil {
		t.Fatal("artifact metadata = nil, want supplied metadata")
	}
	if got, want := *updated.input.Metadata, json.RawMessage(`{"a":1,"z":{"a":1,"b":2}}`); !reflect.DeepEqual(got, want) {
		t.Fatalf("artifact metadata = %s, want %s", got, want)
	}
}

func TestTaskUpdatePrepareCallPreservesOmissionAndMetadataClear(t *testing.T) {
	taskID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	taskTool := newTaskUpdate(newStore(nil))
	tests := []struct {
		name             string
		raw              string
		wantMetadata     *json.RawMessage
		wantSubjectValue string
	}{
		{name: "omitted fields", raw: `{"taskId":"` + taskID + `"}`},
		{name: "empty scalar values are present", raw: `{"taskId":"` + taskID + `","subject":"","description":"","activeForm":"","status":"pending"}`, wantSubjectValue: ""},
		{name: "empty metadata clears", raw: `{"taskId":"` + taskID + `","metadata":{}}`, wantMetadata: rawMessagePointer(json.RawMessage(`{}`))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, artifact, err := taskTool.PrepareCall(context.Background(), uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"), tt.raw)
			if err != nil {
				t.Fatalf("PrepareCall() error = %v", err)
			}
			updated := artifact.(*taskUpdateArtifact)
			if tt.wantSubjectValue != "" || strings.Contains(tt.name, "empty scalar") {
				if updated.input.Subject == nil || *updated.input.Subject != tt.wantSubjectValue {
					t.Fatalf("subject pointer = %#v, want present empty value", updated.input.Subject)
				}
				if updated.input.Description == nil || *updated.input.Description != "" {
					t.Fatalf("description pointer = %#v, want present empty value", updated.input.Description)
				}
				if updated.input.ActiveForm == nil || *updated.input.ActiveForm != "" {
					t.Fatalf("active form pointer = %#v, want present empty value", updated.input.ActiveForm)
				}
				if updated.input.Status == nil || *updated.input.Status != StatusPending {
					t.Fatalf("status pointer = %#v, want pending", updated.input.Status)
				}
			} else {
				if updated.input.Subject != nil || updated.input.Description != nil || updated.input.ActiveForm != nil || updated.input.Status != nil {
					t.Fatalf("omitted scalar pointers = subject %#v description %#v activeForm %#v status %#v, want nil", updated.input.Subject, updated.input.Description, updated.input.ActiveForm, updated.input.Status)
				}
			}
			if !reflect.DeepEqual(updated.input.Metadata, tt.wantMetadata) {
				t.Fatalf("metadata pointer = %v, want %v", updated.input.Metadata, tt.wantMetadata)
			}
		})
	}
}

func rawMessagePointer(value json.RawMessage) *json.RawMessage {
	value = cloneRawMessage(value)
	return &value
}

func TestTaskUpdatePrepareRejectsEveryInvalidArgument(t *testing.T) {
	validTaskID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	valid := `{"taskId":"` + validTaskID + `"}`
	tooManyIDs := make([]string, maxDependencies+1)
	for index := range tooManyIDs {
		tooManyIDs[index] = fmt.Sprintf("00000000-0000-4000-8000-%012x", index+1)
	}
	encodedTooManyIDs, err := json.Marshal(tooManyIDs)
	if err != nil {
		t.Fatalf("json.Marshal(tooManyIDs) error = %v", err)
	}
	metadataTooLarge := `{"metadata":{"value":"` + strings.Repeat("m", maxMetadataBytes-10) + `"}}`
	overSized := strings.Repeat("x", maxTaskArgsBytes+1)
	tests := []struct {
		name string
		raw  string
	}{
		{name: "malformed", raw: `{"taskId":`},
		{name: "trailing JSON", raw: valid + ` {}`},
		{name: "null root", raw: `null`},
		{name: "array root", raw: `[]`},
		{name: "scalar root", raw: `"` + validTaskID + `"`},
		{name: "missing taskId", raw: `{}`},
		{name: "whitespace taskId", raw: `{"taskId":" \t\n "}`},
		{name: "empty taskId", raw: `{"taskId":""}`},
		{name: "invalid taskId UUID", raw: `{"taskId":"not-a-uuid"}`},
		{name: "zero taskId UUID", raw: `{"taskId":"00000000-0000-0000-0000-000000000000"}`},
		{name: "taskId wrong type", raw: `{"taskId":7}`},
		{name: "taskId null", raw: `{"taskId":null}`},
		{name: "taskId array", raw: `{"taskId":[]}`},
		{name: "taskId object", raw: `{"taskId":{}}`},
		{name: "unknown field", raw: `{"taskId":"` + validTaskID + `","unexpected":true}`},
		{name: "case variant field", raw: `{"TaskId":"` + validTaskID + `"}`},
		{name: "duplicate field", raw: `{"taskId":"` + validTaskID + `","taskId":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"}`},
		{name: "subject wrong type", raw: `{"taskId":"` + validTaskID + `","subject":7}`},
		{name: "subject null", raw: `{"taskId":"` + validTaskID + `","subject":null}`},
		{name: "description wrong type", raw: `{"taskId":"` + validTaskID + `","description":[]}`},
		{name: "description null", raw: `{"taskId":"` + validTaskID + `","description":null}`},
		{name: "active form wrong type", raw: `{"taskId":"` + validTaskID + `","activeForm":7}`},
		{name: "active form null", raw: `{"taskId":"` + validTaskID + `","activeForm":null}`},
		{name: "status wrong type", raw: `{"taskId":"` + validTaskID + `","status":7}`},
		{name: "status null", raw: `{"taskId":"` + validTaskID + `","status":null}`},
		{name: "status unknown", raw: `{"taskId":"` + validTaskID + `","status":"paused"}`},
		{name: "status case variant", raw: `{"taskId":"` + validTaskID + `","status":"Completed"}`},
		{name: "add dependency wrong type", raw: `{"taskId":"` + validTaskID + `","addBlockedBy":"id"}`},
		{name: "add dependency null", raw: `{"taskId":"` + validTaskID + `","addBlockedBy":null}`},
		{name: "add dependency item wrong type", raw: `{"taskId":"` + validTaskID + `","addBlockedBy":[7]}`},
		{name: "add dependency item null", raw: `{"taskId":"` + validTaskID + `","addBlockedBy":[null]}`},
		{name: "add dependency invalid UUID", raw: `{"taskId":"` + validTaskID + `","addBlockedBy":["not-a-uuid"]}`},
		{name: "add dependency zero UUID", raw: `{"taskId":"` + validTaskID + `","addBlockedBy":["00000000-0000-0000-0000-000000000000"]}`},
		{name: "remove dependency wrong type", raw: `{"taskId":"` + validTaskID + `","removeBlockedBy":{}}`},
		{name: "remove dependency null", raw: `{"taskId":"` + validTaskID + `","removeBlockedBy":null}`},
		{name: "remove dependency item wrong type", raw: `{"taskId":"` + validTaskID + `","removeBlockedBy":[false]}`},
		{name: "remove dependency item null", raw: `{"taskId":"` + validTaskID + `","removeBlockedBy":[null]}`},
		{name: "remove dependency invalid UUID", raw: `{"taskId":"` + validTaskID + `","removeBlockedBy":["not-a-uuid"]}`},
		{name: "metadata wrong type", raw: `{"taskId":"` + validTaskID + `","metadata":[]}`},
		{name: "metadata null", raw: `{"taskId":"` + validTaskID + `","metadata":null}`},
		{name: "metadata scalar", raw: `{"taskId":"` + validTaskID + `","metadata":"value"}`},
		{name: "metadata duplicate nested key", raw: `{"taskId":"` + validTaskID + `","metadata":{"key":1,"key":2}}`},
		{name: "too many add dependencies", raw: `{"taskId":"` + validTaskID + `","addBlockedBy":` + string(encodedTooManyIDs) + `}`},
		{name: "too many remove dependencies", raw: `{"taskId":"` + validTaskID + `","removeBlockedBy":` + string(encodedTooManyIDs) + `}`},
		{name: "subject byte limit", raw: `{"taskId":"` + validTaskID + `","subject":"` + strings.Repeat("s", maxSubjectBytes+1) + `"}`},
		{name: "description byte limit", raw: `{"taskId":"` + validTaskID + `","description":"` + strings.Repeat("d", maxDescriptionBytes+1) + `"}`},
		{name: "active form byte limit", raw: `{"taskId":"` + validTaskID + `","activeForm":"` + strings.Repeat("a", maxActiveFormBytes+1) + `"}`},
		{name: "metadata byte limit", raw: metadataTooLarge},
		{name: "raw argument byte limit", raw: overSized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			taskTool := newTaskUpdate(newStore(nil))
			_, artifact, err := taskTool.PrepareCall(context.Background(), uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"), tt.raw)
			if artifact != nil {
				t.Fatalf("invalid preparation returned artifact %T", artifact)
			}
			assertPrepareError(t, err)
		})
	}
}

func TestTaskUpdateArtifactsOwnMutableValues(t *testing.T) {
	taskTool := newTaskUpdate(newStore(nil))
	executionID := uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	raw := `{"taskId":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","subject":"subject","description":"description","activeForm":"Working","status":"completed","addBlockedBy":["bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"],"removeBlockedBy":["dddddddd-dddd-4ddd-8ddd-dddddddddddd"],"metadata":{"key":"value"}}`
	_, firstArtifact, err := taskTool.PrepareCall(context.Background(), executionID, raw)
	if err != nil {
		t.Fatalf("first PrepareCall() error = %v", err)
	}
	_, secondArtifact, err := taskTool.PrepareCall(context.Background(), executionID, raw)
	if err != nil {
		t.Fatalf("second PrepareCall() error = %v", err)
	}
	first := firstArtifact.(*taskUpdateArtifact)
	second := secondArtifact.(*taskUpdateArtifact)
	*first.input.Subject = "changed"
	*first.input.Description = "changed"
	*first.input.ActiveForm = "changed"
	*first.input.Status = StatusPending
	first.input.AddBlockedBy[0] = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	first.input.RemoveBlockedBy[0] = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	(*first.input.Metadata)[0] = '['

	if got, want := *second.input.Subject, "subject"; got != want {
		t.Fatalf("second subject = %q, want %q", got, want)
	}
	if got, want := *second.input.Description, "description"; got != want {
		t.Fatalf("second description = %q, want %q", got, want)
	}
	if got, want := *second.input.ActiveForm, "Working"; got != want {
		t.Fatalf("second active form = %q, want %q", got, want)
	}
	if got, want := *second.input.Status, StatusCompleted; got != want {
		t.Fatalf("second status = %q, want %q", got, want)
	}
	if got, want := second.input.AddBlockedBy, []string{"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("second add dependencies = %#v, want %#v", got, want)
	}
	if got, want := second.input.RemoveBlockedBy, []string{"dddddddd-dddd-4ddd-8ddd-dddddddddddd"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("second remove dependencies = %#v, want %#v", got, want)
	}
	if got, want := string(*second.input.Metadata), `{"key":"value"}`; got != want {
		t.Fatalf("second metadata = %q, want %q", got, want)
	}
}

func TestTaskUpdateExecutionPatchesPresentFieldsAndUsesPreparedArtifact(t *testing.T) {
	taskID := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	store := newStore(sequenceIDs(taskID))
	input := createInput{
		Subject:     "original subject",
		Description: "original description",
		ActiveForm:  "original active form",
		Metadata:    json.RawMessage(`{"owner":"original"}`),
	}
	mustCreate(t, store, input)
	taskTool := newTaskUpdate(store)
	_, artifact, err := taskTool.PrepareCall(context.Background(), uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"), `{"taskId":"AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA","subject":"changed subject","metadata":{"z":2,"a":1}}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	result, err := taskTool.InvokableRun(preparedContext(artifact), `{"taskId":"`+taskID.String()+`","subject":"raw subject","status":"deleted","metadata":{}}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v, want nil", err)
	}
	want := `{"task":{"id":"` + taskID.String() + `","subject":"changed subject","description":"original description","activeForm":"original active form","status":"pending","metadata":{"a":1,"z":2}}}`
	if got := resultText(t, result); got != want {
		t.Fatalf("ordinary result = %q, want exact %q", got, want)
	}
	updated := taskEnvelope(t, result)
	if updated.Subject != "changed subject" || updated.Description != "original description" || updated.ActiveForm != "original active form" {
		t.Fatalf("updated scalar fields = %#v, want presence-aware patch", updated)
	}
	if got, want := string(updated.Metadata), `{"a":1,"z":2}`; got != want {
		t.Fatalf("updated metadata = %q, want replacement metadata %q", got, want)
	}
}

func TestTaskUpdateDependencyPatchNormalizesAndRemovalWins(t *testing.T) {
	firstDependencyID := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	secondDependencyID := uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	overlapDependencyID := uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	taskID := uuid.MustParse("dddddddd-dddd-4ddd-8ddd-dddddddddddd")
	store := newStore(sequenceIDs(firstDependencyID, secondDependencyID, overlapDependencyID, taskID))
	for _, subject := range []string{"first", "second", "overlap"} {
		mustCreate(t, store, validCreateInput(subject))
	}
	targetInput := validCreateInput("target")
	targetInput.BlockedBy = []string{overlapDependencyID.String()}
	mustCreate(t, store, targetInput)
	taskTool := newTaskUpdate(store)
	raw := `{"taskId":"` + taskID.String() + `","addBlockedBy":["` + strings.ToUpper(secondDependencyID.String()) + `","` + firstDependencyID.String() + `","` + overlapDependencyID.String() + `","` + secondDependencyID.String() + `"],"removeBlockedBy":["` + strings.ToUpper(overlapDependencyID.String()) + `","` + overlapDependencyID.String() + `"]}`
	_, artifact, err := taskTool.PrepareCall(context.Background(), uuid.MustParse("eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"), raw)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	result, err := taskTool.InvokableRun(preparedContext(artifact), `{"taskId":"`+taskID.String()+`","addBlockedBy":[]}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v, want nil", err)
	}
	updated := taskEnvelope(t, result)
	if got, want := updated.BlockedBy, []string{firstDependencyID.String(), secondDependencyID.String()}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updated dependencies = %#v, want sorted %#v with removal winning", got, want)
	}
}

func TestTaskUpdateDeletedResultIsExactAndRemovesInboundReferences(t *testing.T) {
	victimID := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	dependentID := uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	store := newStore(sequenceIDs(victimID, dependentID))
	mustCreate(t, store, validCreateInput("victim"))
	dependentInput := validCreateInput("dependent")
	dependentInput.BlockedBy = []string{victimID.String()}
	mustCreate(t, store, dependentInput)
	taskTool := newTaskUpdate(store)
	_, artifact, err := taskTool.PrepareCall(context.Background(), uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc"), `{"taskId":"`+victimID.String()+`","status":"deleted"}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	result, err := taskTool.InvokableRun(preparedContext(artifact), `{"taskId":"`+dependentID.String()+`","status":"completed"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v, want nil", err)
	}
	if got, want := resultText(t, result), `{"deletedTaskId":"`+victimID.String()+`"}`; got != want {
		t.Fatalf("deleted result = %q, want exact %q", got, want)
	}
	if got := store.list(); len(got) != 1 || got[0].ID != dependentID.String() || got[0].BlockedBy != nil {
		t.Fatalf("graph after deletion = %#v, want only unblocked dependent", got)
	}
}

func assertTaskUpdateLiveFailure(t *testing.T, taskTool *TaskUpdate, artifact tool.PreparedArtifact, raw, secret string) {
	t.Helper()
	result, err := taskTool.InvokableRun(preparedContext(artifact), raw)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v, want nil model-visible result", err)
	}
	text := resultText(t, result)
	if !strings.HasPrefix(text, "error:") {
		t.Fatalf("live failure result = %q, want model-visible error", text)
	}
	if secret != "" && strings.Contains(text, secret) {
		t.Fatalf("live failure result echoed task text or metadata %q: %q", secret, text)
	}
}

func TestTaskUpdateUnknownIDAndDependencyFailuresAreModelVisible(t *testing.T) {
	t.Run("unknown task ID", func(t *testing.T) {
		taskTool := newTaskUpdate(newStore(nil))
		_, artifact, err := taskTool.PrepareCall(context.Background(), uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"), `{"taskId":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","subject":"secret subject"}`)
		if err != nil {
			t.Fatalf("PrepareCall() error = %v", err)
		}
		assertTaskUpdateLiveFailure(t, taskTool, artifact, `{"taskId":"changed","subject":"raw"}`, "secret subject")
	})

	for _, remove := range []bool{false, true} {
		name := "unknown add dependency"
		field := "addBlockedBy"
		if remove {
			name = "unknown remove dependency"
			field = "removeBlockedBy"
		}
		t.Run(name, func(t *testing.T) {
			taskID := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
			store := newStore(sequenceIDs(taskID))
			input := validCreateInput("secret subject")
			input.Metadata = json.RawMessage(`{"secret":"hidden metadata"}`)
			mustCreate(t, store, input)
			taskTool := newTaskUpdate(store)
			raw := `{"taskId":"` + taskID.String() + `","` + field + `":["bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"]}`
			_, artifact, err := taskTool.PrepareCall(context.Background(), uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc"), raw)
			if err != nil {
				t.Fatalf("PrepareCall() error = %v", err)
			}
			assertTaskUpdateLiveFailure(t, taskTool, artifact, `{"taskId":"`+taskID.String()+`","subject":"raw"}`, "secret subject")
		})
	}
}

func TestTaskUpdateSelfDependencyAndCycleFailuresAreModelVisible(t *testing.T) {
	t.Run("self dependency", func(t *testing.T) {
		taskID := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
		store := newStore(sequenceIDs(taskID))
		mustCreate(t, store, validCreateInput("secret subject"))
		taskTool := newTaskUpdate(store)
		_, artifact, err := taskTool.PrepareCall(context.Background(), uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"), `{"taskId":"`+taskID.String()+`","addBlockedBy":["`+taskID.String()+`"]}`)
		if err != nil {
			t.Fatalf("PrepareCall() error = %v", err)
		}
		assertTaskUpdateLiveFailure(t, taskTool, artifact, `{"taskId":"`+taskID.String()+`","status":"completed"}`, "secret subject")
	})

	t.Run("cycle", func(t *testing.T) {
		firstID := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
		secondID := uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
		store := newStore(sequenceIDs(firstID, secondID))
		mustCreate(t, store, validCreateInput("first"))
		mustCreate(t, store, validCreateInput("second"))
		taskTool := newTaskUpdate(store)
		_, firstArtifact, err := taskTool.PrepareCall(context.Background(), uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc"), `{"taskId":"`+firstID.String()+`","addBlockedBy":["`+secondID.String()+`"]}`)
		if err != nil {
			t.Fatalf("first PrepareCall() error = %v", err)
		}
		firstResult, err := taskTool.InvokableRun(preparedContext(firstArtifact), `null`)
		if err != nil {
			t.Fatalf("first cycle-edge InvokableRun() error = %v", err)
		}
		if strings.HasPrefix(resultText(t, firstResult), "error:") {
			t.Fatalf("first cycle edge failed: %s", resultText(t, firstResult))
		}
		_, secondArtifact, err := taskTool.PrepareCall(context.Background(), uuid.MustParse("dddddddd-dddd-4ddd-8ddd-dddddddddddd"), `{"taskId":"`+secondID.String()+`","addBlockedBy":["`+firstID.String()+`"]}`)
		if err != nil {
			t.Fatalf("second PrepareCall() error = %v", err)
		}
		assertTaskUpdateLiveFailure(t, taskTool, secondArtifact, `{"taskId":"`+secondID.String()+`","subject":"raw"}`, "")
	})
}

func TestTaskUpdateStateInvariantFailuresAreModelVisible(t *testing.T) {
	t.Run("blocked task cannot become in progress", func(t *testing.T) {
		blockerID := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
		taskID := uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
		store := newStore(sequenceIDs(blockerID, taskID))
		mustCreate(t, store, validCreateInput("blocker"))
		input := validCreateInput("secret blocked task")
		input.BlockedBy = []string{blockerID.String()}
		mustCreate(t, store, input)
		taskTool := newTaskUpdate(store)
		_, artifact, err := taskTool.PrepareCall(context.Background(), uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc"), `{"taskId":"`+taskID.String()+`","status":"in_progress"}`)
		if err != nil {
			t.Fatalf("PrepareCall() error = %v", err)
		}
		assertTaskUpdateLiveFailure(t, taskTool, artifact, `{"taskId":"`+taskID.String()+`","status":"completed"}`, "secret blocked task")
	})

	t.Run("in-progress task cannot acquire incomplete blocker", func(t *testing.T) {
		taskID := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
		blockerID := uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
		store := newStore(sequenceIDs(taskID, blockerID))
		mustCreate(t, store, validCreateInput("secret in progress task"))
		taskTool := newTaskUpdate(store)
		_, startArtifact, err := taskTool.PrepareCall(context.Background(), uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc"), `{"taskId":"`+taskID.String()+`","status":"in_progress"}`)
		if err != nil {
			t.Fatalf("start PrepareCall() error = %v", err)
		}
		startResult, err := taskTool.InvokableRun(preparedContext(startArtifact), `null`)
		if err != nil || strings.HasPrefix(resultText(t, startResult), "error:") {
			t.Fatalf("starting task failed: result=%s error=%v", resultText(t, startResult), err)
		}
		mustCreate(t, store, validCreateInput("incomplete blocker"))
		_, artifact, err := taskTool.PrepareCall(context.Background(), uuid.MustParse("dddddddd-dddd-4ddd-8ddd-dddddddddddd"), `{"taskId":"`+taskID.String()+`","addBlockedBy":["`+blockerID.String()+`"]}`)
		if err != nil {
			t.Fatalf("dependency PrepareCall() error = %v", err)
		}
		assertTaskUpdateLiveFailure(t, taskTool, artifact, `{"taskId":"`+taskID.String()+`","status":"completed"}`, "secret in progress task")
	})
}

func TestTaskUpdateAggregateLimitFailureIsModelVisible(t *testing.T) {
	ids := idsFor(127)
	store := newStore(sequenceIDs(ids...))
	for range ids {
		mustCreate(t, store, createInput{
			Subject:     "s",
			Description: strings.Repeat("d", maxDescriptionBytes),
		})
	}
	taskTool := newTaskUpdate(store)
	metadata := `{"m":"` + strings.Repeat("m", maxMetadataBytes-8) + `"}`
	raw := `{"taskId":"` + ids[0].String() + `","metadata":` + metadata + `}`
	_, artifact, err := taskTool.PrepareCall(context.Background(), uuid.MustParse("eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"), raw)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	assertTaskUpdateLiveFailure(t, taskTool, artifact, `{"taskId":"`+ids[0].String()+`","metadata":{}}`, "")
}

func TestTaskUpdateRejectsMissingNilAndCrossToolArtifacts(t *testing.T) {
	store := newStore(nil)
	taskTool := newTaskUpdate(store)
	getTool := newTaskGet(store)
	_, getArtifact, err := getTool.PrepareCall(context.Background(), uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"), `{"taskId":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}`)
	if err != nil {
		t.Fatalf("TaskGet PrepareCall() error = %v", err)
	}
	var nilArtifact *taskUpdateArtifact
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
			result, err := taskTool.InvokableRun(tt.context, `{"taskId":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","status":"deleted"}`)
			if err != nil {
				t.Fatalf("InvokableRun() error = %v, want nil", err)
			}
			if text := resultText(t, result); !strings.HasPrefix(text, "error:") {
				t.Fatalf("artifact rejection result = %q, want model-visible error", text)
			}
		})
	}
}

func TestTaskUpdateSequentialAndAuditSummary(t *testing.T) {
	taskTool := newTaskUpdate(newStore(nil))
	if !taskTool.Sequential() {
		t.Fatal("Sequential() = false, want true")
	}
	if got, want := taskTool.AuditSummary(`{"taskId":"secret","subject":"secret"}`), "TaskUpdate"; got != want {
		t.Fatalf("AuditSummary() = %q, want %q", got, want)
	}
	var _ tool.InvokableTool = taskTool
	var _ tool.CallPreparer = taskTool
	var _ tool.Auditable = taskTool
	var _ tool.Sequential = taskTool
	var _ tool.PreparedArtifact = (*taskUpdateArtifact)(nil)
}

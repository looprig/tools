package task

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

const wantTaskCreateSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "subject": {"type": "string", "minLength": 1, "description": "Brief task title."},
    "description": {"type": "string", "minLength": 1, "description": "Detailed task requirements."},
    "activeForm": {"type": "string", "description": "Present-continuous text shown while the task is active."},
    "blockedBy": {"type": "array", "items": {"type": "string"}, "maxItems": 128, "description": "Existing task UUIDs that must complete first."},
    "metadata": {"type": "object", "description": "Optional structured task metadata."}
  },
  "required": ["subject", "description"]
}`

func preparedContext(artifact tool.PreparedArtifact) context.Context {
	return loop.WithPreparedCall(context.Background(), tool.PreparedCall{Artifact: artifact})
}

func resultText(t *testing.T, result *tool.ToolResult) string {
	t.Helper()
	if result == nil {
		t.Fatal("tool returned nil result")
	}
	if len(result.Content) != 1 {
		t.Fatalf("tool returned %d content blocks, want exactly one", len(result.Content))
	}
	block, ok := result.Content[0].(*content.TextBlock)
	if !ok {
		t.Fatalf("tool returned %T content block, want *content.TextBlock", result.Content[0])
	}
	return block.Text
}

func assertJSONEqual(t *testing.T, got, want string) {
	t.Helper()
	var gotValue any
	if err := json.Unmarshal([]byte(got), &gotValue); err != nil {
		t.Fatalf("got invalid JSON %q: %v", got, err)
	}
	var wantValue any
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("test expectation is invalid JSON %q: %v", want, err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("JSON value = %s, want %s", got, want)
	}
}

func taskEnvelope(t *testing.T, result *tool.ToolResult) Task {
	t.Helper()
	text := resultText(t, result)
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatalf("result JSON = %q: %v", text, err)
	}
	if len(envelope) != 1 {
		t.Fatalf("result envelope keys = %#v, want exactly task", envelope)
	}
	taskJSON, ok := envelope["task"]
	if !ok {
		t.Fatalf("result envelope = %#v, want task key", envelope)
	}
	var value Task
	if err := json.Unmarshal(taskJSON, &value); err != nil {
		t.Fatalf("task JSON = %s: %v", taskJSON, err)
	}
	return value
}

func assertPrepareError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("preparation succeeded, want error")
	}
	var typed *prepareError
	if !reflectAs(err, &typed) {
		t.Fatalf("preparation error type = %T, want *prepareError", err)
	}
	if got, want := err.Error(), prepareErrorText; got != want {
		t.Fatalf("preparation error text = %q, want %q", got, want)
	}
}

// reflectAs keeps the test assertions independent of the concrete wrapping
// details while still requiring the package's stable private preparation type.
func reflectAs(err error, target **prepareError) bool {
	if typed, ok := err.(*prepareError); ok {
		*target = typed
		return true
	}
	return false
}

func TestTaskCreateInfoExactContract(t *testing.T) {
	taskTool := newTaskCreate(newStore(nil))
	info, err := taskTool.Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if info == nil {
		t.Fatal("Info() returned nil info")
	}
	if got, want := info.Name, "TaskCreate"; got != want {
		t.Fatalf("Info().Name = %q, want %q", got, want)
	}
	if got, want := info.Desc, "Create one task in this Loop's private task graph."; got != want {
		t.Fatalf("Info().Desc = %q, want %q", got, want)
	}
	assertJSONEqual(t, string(info.Schema), wantTaskCreateSchema)
}

func TestTaskCreatePrepareCallReturnsPureTypedArtifact(t *testing.T) {
	firstID := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	secondID := uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	executionID := uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	taskTool := newTaskCreate(newStore(nil))
	raw := `{"subject":"  Build parser  ","description":"Detailed requirements","activeForm":"Building parser","blockedBy":["BBBBBBBB-BBBB-4BBB-8BBB-BBBBBBBBBBBB","` + firstID.String() + `","` + secondID.String() + `","` + firstID.String() + `"],"metadata":{"z":{"b":2,"a":1},"a":1}}`

	request, artifact, err := taskTool.PrepareCall(context.Background(), executionID, raw)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	if want := (tool.Request{ToolName: "TaskCreate"}); !reflect.DeepEqual(request, want) {
		t.Fatalf("request = %#v, want %#v", request, want)
	}
	if artifact == nil {
		t.Fatal("PrepareCall() returned nil artifact")
	}
	created, ok := artifact.(*taskCreateArtifact)
	if !ok || created == nil {
		t.Fatalf("artifact = %T, want non-nil *taskCreateArtifact", artifact)
	}
	if got, want := created.Token, executionID.String(); got != want {
		t.Fatalf("artifact token = %q, want %q", got, want)
	}
	wantBlockedBy := []string{firstID.String(), secondID.String()}
	if !reflect.DeepEqual(created.input.BlockedBy, wantBlockedBy) {
		t.Fatalf("artifact dependencies = %#v, want %#v", created.input.BlockedBy, wantBlockedBy)
	}
	if got, want := created.input.Metadata, json.RawMessage(`{"a":1,"z":{"a":1,"b":2}}`); !reflect.DeepEqual(got, want) {
		t.Fatalf("artifact metadata = %s, want %s", got, want)
	}
	if got, want := created.input.Subject, "  Build parser  "; got != want {
		t.Fatalf("artifact subject = %q, want verbatim %q", got, want)
	}
	if len(request.Requirements) != 0 {
		t.Fatalf("pure request requirements = %#v, want none", request.Requirements)
	}
}

func TestTaskCreatePrepareMetadataOmissionAndEmptyObject(t *testing.T) {
	tests := []struct {
		name         string
		raw          string
		wantMetadata json.RawMessage
	}{
		{name: "omitted", raw: `{"subject":"subject","description":"description"}`},
		{name: "empty object clear sentinel", raw: `{"subject":"subject","description":"description","metadata":{}}`, wantMetadata: json.RawMessage(`{}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			taskTool := newTaskCreate(newStore(nil))
			_, artifact, err := taskTool.PrepareCall(context.Background(), uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"), tt.raw)
			if err != nil {
				t.Fatalf("PrepareCall() error = %v", err)
			}
			created := artifact.(*taskCreateArtifact)
			if !reflect.DeepEqual(created.input.Metadata, tt.wantMetadata) {
				t.Fatalf("artifact metadata = %s, want %s", created.input.Metadata, tt.wantMetadata)
			}
		})
	}
}

func TestTaskCreatePrepareRejectsInvalidArguments(t *testing.T) {
	valid := `{"subject":"subject","description":"description"}`
	tooManyDependencies := make([]string, maxDependencies+1)
	for i := range tooManyDependencies {
		tooManyDependencies[i] = fmt.Sprintf("00000000-0000-4000-8000-%012x", i+1)
	}
	encodedDependencies, err := json.Marshal(tooManyDependencies)
	if err != nil {
		t.Fatalf("json.Marshal(dependencies) error = %v", err)
	}
	metadataTooLarge := `{"subject":"subject","description":"description","metadata":{"value":"` + strings.Repeat("x", maxMetadataBytes) + `"}}`
	overSized := `{"subject":"` + strings.Repeat("x", maxTaskArgsBytes) + `","description":"description"}`

	tests := []struct {
		name string
		raw  string
	}{
		{name: "missing subject", raw: `{"description":"description"}`},
		{name: "missing description", raw: `{"subject":"subject"}`},
		{name: "empty subject", raw: `{"subject":"","description":"description"}`},
		{name: "whitespace subject", raw: `{"subject":" \t\n ","description":"description"}`},
		{name: "empty description", raw: `{"subject":"subject","description":""}`},
		{name: "whitespace description", raw: `{"subject":"subject","description":" \t\n "}`},
		{name: "unknown field", raw: `{"subject":"subject","description":"description","extra":true}`},
		{name: "case variant field", raw: `{"Subject":"subject","description":"description"}`},
		{name: "duplicate field", raw: `{"subject":"one","subject":"two","description":"description"}`},
		{name: "malformed", raw: `{"subject":"subject","description":`},
		{name: "trailing JSON", raw: valid + ` {}`},
		{name: "null root", raw: `null`},
		{name: "array root", raw: `[]`},
		{name: "scalar root", raw: `"subject"`},
		{name: "subject wrong type", raw: `{"subject":7,"description":"description"}`},
		{name: "subject null", raw: `{"subject":null,"description":"description"}`},
		{name: "description wrong type", raw: `{"subject":"subject","description":[]}`},
		{name: "description null", raw: `{"subject":"subject","description":null}`},
		{name: "active form wrong type", raw: `{"subject":"subject","description":"description","activeForm":7}`},
		{name: "active form null", raw: `{"subject":"subject","description":"description","activeForm":null}`},
		{name: "blockedBy wrong type", raw: `{"subject":"subject","description":"description","blockedBy":"id"}`},
		{name: "blockedBy null", raw: `{"subject":"subject","description":"description","blockedBy":null}`},
		{name: "blockedBy item wrong type", raw: `{"subject":"subject","description":"description","blockedBy":[7]}`},
		{name: "blockedBy item null", raw: `{"subject":"subject","description":"description","blockedBy":[null]}`},
		{name: "metadata wrong type", raw: `{"subject":"subject","description":"description","metadata":[]}`},
		{name: "metadata null", raw: `{"subject":"subject","description":"description","metadata":null}`},
		{name: "metadata scalar", raw: `{"subject":"subject","description":"description","metadata":"value"}`},
		{name: "metadata duplicate nested key", raw: `{"subject":"subject","description":"description","metadata":{"key":1,"key":2}}`},
		{name: "invalid dependency", raw: `{"subject":"subject","description":"description","blockedBy":["not-a-uuid"]}`},
		{name: "zero dependency UUID", raw: `{"subject":"subject","description":"description","blockedBy":["00000000-0000-0000-0000-000000000000"]}`},
		{name: "too many dependencies", raw: `{"subject":"subject","description":"description","blockedBy":` + string(encodedDependencies) + `}`},
		{name: "subject byte limit", raw: `{"subject":"` + strings.Repeat("x", maxSubjectBytes+1) + `","description":"description"}`},
		{name: "description byte limit", raw: `{"subject":"subject","description":"` + strings.Repeat("x", maxDescriptionBytes+1) + `"}`},
		{name: "active form byte limit", raw: `{"subject":"subject","description":"description","activeForm":"` + strings.Repeat("x", maxActiveFormBytes+1) + `"}`},
		{name: "metadata byte limit", raw: metadataTooLarge},
		{name: "raw argument byte limit", raw: overSized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			taskTool := newTaskCreate(newStore(nil))
			_, artifact, err := taskTool.PrepareCall(context.Background(), uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"), tt.raw)
			if artifact != nil {
				t.Fatalf("invalid preparation returned artifact %T", artifact)
			}
			assertPrepareError(t, err)
		})
	}
}

func TestTaskCreatePrepareOwnsMutableArtifactData(t *testing.T) {
	taskTool := newTaskCreate(newStore(nil))
	_, artifact, err := taskTool.PrepareCall(context.Background(), uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"), `{"subject":"subject","description":"description","blockedBy":["bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"],"metadata":{"key":"value"}}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	created := artifact.(*taskCreateArtifact)
	wantDependencies := append([]string(nil), created.input.BlockedBy...)
	wantMetadata := append(json.RawMessage(nil), created.input.Metadata...)
	created.input.BlockedBy[0] = "changed"
	created.input.Metadata[0] = '['
	if !reflect.DeepEqual(wantDependencies, []string{"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"}) {
		t.Fatalf("test dependency snapshot = %#v, want canonical dependency", wantDependencies)
	}
	if got, want := string(wantMetadata), `{"key":"value"}`; got != want {
		t.Fatalf("test metadata snapshot = %q, want %q", got, want)
	}
}

func TestTaskCreateExecutionReturnsCompleteStructuredTask(t *testing.T) {
	id := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	store := newStore(sequenceIDs(id))
	taskTool := newTaskCreate(store)
	raw := `{"subject":"subject","description":"description","activeForm":"Working on task","metadata":{"z":2,"a":1}}`
	_, artifact, err := taskTool.PrepareCall(context.Background(), uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"), raw)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	result, err := taskTool.InvokableRun(preparedContext(artifact), `{"subject":"changed","description":"changed"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v, want nil", err)
	}
	created := taskEnvelope(t, result)
	if got, want := created.ID, id.String(); got != want {
		t.Fatalf("created ID = %q, want %q", got, want)
	}
	if created.Subject != "subject" || created.Description != "description" || created.ActiveForm != "Working on task" {
		t.Fatalf("created task fields = %#v, want prepared values", created)
	}
	if created.Status != StatusPending {
		t.Fatalf("created status = %q, want %q", created.Status, StatusPending)
	}
	if got, want := string(created.Metadata), `{"a":1,"z":2}`; got != want {
		t.Fatalf("created metadata = %q, want %q", got, want)
	}
	if created.BlockedBy != nil || created.Blocks != nil {
		t.Fatalf("created dependency fields = blockedBy %#v blocks %#v, want omitted", created.BlockedBy, created.Blocks)
	}
}

func TestTaskCreateExecutionLiveFailureIsModelVisibleAndDoesNotEchoTaskData(t *testing.T) {
	store := newStore(sequenceIDs(uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")))
	taskTool := newTaskCreate(store)
	secretSubject := "secret subject that must not be echoed"
	secretMetadata := `{"secret":"metadata that must not be echoed"}`
	raw := `{"subject":"` + secretSubject + `","description":"description","blockedBy":["bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"],"metadata":` + secretMetadata + `}`
	_, artifact, err := taskTool.PrepareCall(context.Background(), uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc"), raw)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	result, err := taskTool.InvokableRun(preparedContext(artifact), raw)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v, want nil", err)
	}
	text := resultText(t, result)
	if !strings.HasPrefix(text, "error:") {
		t.Fatalf("live failure text = %q, want model-visible error", text)
	}
	if strings.Contains(text, secretSubject) || strings.Contains(text, secretMetadata) {
		t.Fatalf("live failure echoed task text or metadata: %q", text)
	}
	if got := store.list(); len(got) != 0 {
		t.Fatalf("failed create changed graph: %#v", got)
	}
}

func TestTaskCreateRejectsMissingNilAndCrossToolArtifacts(t *testing.T) {
	store := newStore(sequenceIDs(uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")))
	taskTool := newTaskCreate(store)
	getTool := newTaskGet(store)
	_, getArtifact, err := getTool.PrepareCall(context.Background(), uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"), `{"taskId":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}`)
	if err != nil {
		t.Fatalf("TaskGet PrepareCall() error = %v", err)
	}
	var nilArtifact *taskCreateArtifact
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
			result, err := taskTool.InvokableRun(tt.context, `{"subject":"subject","description":"description"}`)
			if err != nil {
				t.Fatalf("InvokableRun() error = %v, want nil", err)
			}
			text := resultText(t, result)
			if !strings.HasPrefix(text, "error:") {
				t.Fatalf("artifact rejection text = %q, want model-visible error", text)
			}
		})
	}
	if got := store.list(); len(got) != 0 {
		t.Fatalf("artifact rejection changed graph: %#v", got)
	}
}

func TestTaskCreateSequentialAndAuditSummary(t *testing.T) {
	taskTool := newTaskCreate(newStore(nil))
	if !taskTool.Sequential() {
		t.Fatal("Sequential() = false, want true")
	}
	if got, want := taskTool.AuditSummary(`{"subject":"secret","description":"secret"}`), "TaskCreate"; got != want {
		t.Fatalf("AuditSummary() = %q, want %q", got, want)
	}
	var _ tool.InvokableTool = taskTool
	var _ tool.CallPreparer = taskTool
	var _ tool.Auditable = taskTool
	var _ tool.Sequential = taskTool
}

func TestTaskCreatePreparedDependenciesAreSorted(t *testing.T) {
	ids := []string{
		"dddddddd-dddd-4ddd-8ddd-dddddddddddd",
		"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		"cccccccc-cccc-4ccc-8ccc-cccccccccccc",
	}
	taskTool := newTaskCreate(newStore(nil))
	raw := `{"subject":"subject","description":"description","blockedBy":["` + strings.ToUpper(ids[0]) + `","` + ids[1] + `","` + ids[2] + `"]}`
	_, artifact, err := taskTool.PrepareCall(context.Background(), uuid.MustParse("eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"), raw)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	want := append([]string(nil), ids...)
	sort.Strings(want)
	if got := artifact.(*taskCreateArtifact).input.BlockedBy; !reflect.DeepEqual(got, want) {
		t.Fatalf("prepared dependencies = %#v, want sorted %#v", got, want)
	}
}

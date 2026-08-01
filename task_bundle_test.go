package tools

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/task"
)

var wantTaskToolNames = []string{"TaskCreate", "TaskUpdate", "TaskGet", "TaskList"}

func TestTaskDefinitionMetadata(t *testing.T) {
	definition := TaskDefinitions()

	if got, want := definition.Name(), "Tasks"; got != want {
		t.Fatalf("Name() = %q, want %q", got, want)
	}
	if got, want := definition.ProducedToolNames(), wantTaskToolNames; !reflect.DeepEqual(got, want) {
		t.Fatalf("ProducedToolNames() = %#v, want %#v", got, want)
	}
	if got := definition.Requirements(); got != 0 {
		t.Fatalf("Requirements() = %v, want zero", got)
	}
}

func TestTaskBundleSharesStoreAndIsolatesBuilds(t *testing.T) {
	definition := TaskDefinitions()
	firstBindings := blueprintBindings()
	firstBindings.LoopID = uuid.MustParse("33333333-3333-4333-8333-333333333333")
	first, err := definition.Build(context.Background(), firstBindings)
	if err != nil {
		t.Fatalf("first Build() error = %v", err)
	}
	if got, want := len(first), len(wantTaskToolNames); got != want {
		t.Fatalf("first Build() returned %d tools, want %d", got, want)
	}
	for index, wantName := range wantTaskToolNames {
		info, err := first[index].Info(context.Background())
		if err != nil {
			t.Fatalf("tool[%d] Info() error = %v", index, err)
		}
		if got := info.Name; got != wantName {
			t.Fatalf("tool[%d] name = %q, want %q", index, got, wantName)
		}
	}

	createResult := runTaskBundleCall(t, first[0], `{"subject":"initial task","description":"shared graph"}`)
	var created struct {
		Task task.Task `json:"task"`
	}
	decodeTaskBundleResult(t, createResult, &created)
	if created.Task.ID == "" {
		t.Fatal("TaskCreate result has empty task ID")
	}

	listResult := runTaskBundleCall(t, first[3], `{}`)
	var listed struct {
		Tasks []task.Task `json:"tasks"`
	}
	decodeTaskBundleResult(t, listResult, &listed)
	if got, want := len(listed.Tasks), 1; got != want {
		t.Fatalf("TaskList returned %d tasks, want %d", got, want)
	}
	if got := listed.Tasks[0].ID; got != created.Task.ID {
		t.Fatalf("TaskList ID = %q, want created ID %q", got, created.Task.ID)
	}

	getResult := runTaskBundleCall(t, first[2], `{"taskId":"`+created.Task.ID+`"}`)
	var fetched struct {
		Task task.Task `json:"task"`
	}
	decodeTaskBundleResult(t, getResult, &fetched)
	if got := fetched.Task.Subject; got != created.Task.Subject {
		t.Fatalf("TaskGet subject = %q, want %q", got, created.Task.Subject)
	}

	runTaskBundleCall(t, first[1], `{"taskId":"`+created.Task.ID+`","subject":"updated task","status":"in_progress"}`)
	updatedResult := runTaskBundleCall(t, first[2], `{"taskId":"`+created.Task.ID+`"}`)
	decodeTaskBundleResult(t, updatedResult, &fetched)
	if got, want := fetched.Task.Subject, "updated task"; got != want {
		t.Fatalf("TaskGet after TaskUpdate subject = %q, want %q", got, want)
	}
	if got, want := fetched.Task.Status, task.StatusInProgress; got != want {
		t.Fatalf("TaskGet after TaskUpdate status = %q, want %q", got, want)
	}

	secondBindings := firstBindings
	secondBindings.LoopID = uuid.MustParse("44444444-4444-4444-8444-444444444444")
	second, err := definition.Build(context.Background(), secondBindings)
	if err != nil {
		t.Fatalf("second Build() error = %v", err)
	}
	secondList := runTaskBundleCall(t, second[3], `{}`)
	listed.Tasks = nil
	decodeTaskBundleResult(t, secondList, &listed)
	if listed.Tasks == nil {
		t.Fatal("second build TaskList returned nil tasks slice")
	}
	if got := len(listed.Tasks); got != 0 {
		t.Fatalf("second build TaskList returned %d tasks, want empty graph", got)
	}
}

func TestTaskBundleToolsHavePreparedAuditedSequentialArtifacts(t *testing.T) {
	built, err := TaskDefinitions().Build(context.Background(), blueprintBindings())
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	argsByName := map[string]string{
		"TaskCreate": `{"subject":"subject","description":"description"}`,
		"TaskUpdate": `{"taskId":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}`,
		"TaskGet":    `{"taskId":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}`,
		"TaskList":   `{}`,
	}
	executionID := uuid.MustParse("55555555-5555-4555-8555-555555555555")
	for index, builtTool := range built {
		info, err := builtTool.Info(context.Background())
		if err != nil {
			t.Fatalf("tool[%d] Info() error = %v", index, err)
		}
		preparer, ok := builtTool.(tool.CallPreparer)
		if !ok {
			t.Errorf("%s does not implement tool.CallPreparer", info.Name)
			continue
		}
		if _, ok := builtTool.(tool.Auditable); !ok {
			t.Errorf("%s does not implement tool.Auditable", info.Name)
		}
		sequential, ok := builtTool.(tool.Sequential)
		if !ok {
			t.Errorf("%s does not implement tool.Sequential", info.Name)
		} else if !sequential.Sequential() {
			t.Errorf("%s Sequential() = false, want true", info.Name)
		}

		_, artifact, err := preparer.PrepareCall(context.Background(), executionID, argsByName[info.Name])
		if err != nil {
			t.Errorf("%s PrepareCall() error = %v", info.Name, err)
			continue
		}
		if artifact == nil || isNilTaskBundleArtifact(artifact) {
			t.Errorf("%s PrepareCall() artifact = %#v, want non-nil typed artifact", info.Name, artifact)
		}
	}
}

func runTaskBundleCall(t *testing.T, builtTool tool.InvokableTool, argsJSON string) *tool.ToolResult {
	t.Helper()
	preparer, ok := builtTool.(tool.CallPreparer)
	if !ok {
		t.Fatalf("built tool %T does not implement tool.CallPreparer", builtTool)
	}
	executionID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	request, artifact, err := preparer.PrepareCall(context.Background(), executionID, argsJSON)
	if err != nil {
		t.Fatalf("%T PrepareCall() error = %v", builtTool, err)
	}
	if artifact == nil || isNilTaskBundleArtifact(artifact) {
		t.Fatalf("%T PrepareCall() returned nil artifact", builtTool)
	}
	ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{
		ExecutionID: executionID,
		Request:     request,
		Artifact:    artifact,
	})
	result, err := builtTool.InvokableRun(ctx, argsJSON)
	if err != nil {
		t.Fatalf("%T InvokableRun() error = %v", builtTool, err)
	}
	return result
}

func isNilTaskBundleArtifact(artifact tool.PreparedArtifact) bool {
	value := reflect.ValueOf(artifact)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func decodeTaskBundleResult(t *testing.T, result *tool.ToolResult, target any) {
	t.Helper()
	if result == nil || len(result.Content) != 1 {
		t.Fatalf("result = %#v, want one content block", result)
	}
	block, ok := result.Content[0].(*content.TextBlock)
	if !ok {
		t.Fatalf("result content type = %T, want *content.TextBlock", result.Content[0])
	}
	if err := json.Unmarshal([]byte(block.Text), target); err != nil {
		t.Fatalf("result %q is not valid JSON: %v", block.Text, err)
	}
}

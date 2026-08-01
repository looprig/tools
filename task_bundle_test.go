package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"sync"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	"github.com/looprig/inference/model"
	"github.com/looprig/inference/stream"
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

func TestTaskBundleSharesStateAcrossModesAndIsolatesLoops(t *testing.T) {
	tasks := TaskDefinitions()
	definition, err := loop.Define(loop.WithName("task-loop"), loop.WithInference(&taskBundleInference{}, taskBundleModel()), loop.WithTools(tasks), loop.WithModes(loop.Mode{Name: "alternate", Tools: []tool.Definition{tasks}}), loop.WithInitialMode("alternate"))
	if err != nil {
		t.Fatalf("loop.Define() error = %v", err)
	}
	base, err := definition.Bind(context.Background(), taskLoopBindings("33333333-3333-4333-8333-333333333333"))
	if err != nil {
		t.Fatalf("base Bind() error = %v", err)
	}
	baseMode, ok := base.Mode("")
	if !ok {
		t.Fatal("base mode missing")
	}
	alternate, err := loop.SelectBoundMode(base, "alternate")
	if err != nil {
		t.Fatalf("SelectBoundMode() error = %v", err)
	}
	alternateMode, ok := alternate.Mode("alternate")
	if !ok {
		t.Fatal("alternate mode missing")
	}
	if got, want := len(baseMode.Tools), len(wantTaskToolNames); got != want {
		t.Fatalf("base mode returned %d tools, want %d", got, want)
	}
	if got, want := len(alternateMode.Tools), len(wantTaskToolNames); got != want {
		t.Fatalf("alternate mode returned %d tools, want %d", got, want)
	}

	createResult := runTaskBundleCall(t, baseMode.Tools[0], `{"subject":"initial task","description":"shared graph"}`)
	var created struct {
		Task task.Task `json:"task"`
	}
	decodeTaskBundleResult(t, createResult, &created)
	if created.Task.ID == "" {
		t.Fatal("TaskCreate result has empty task ID")
	}

	listResult := runTaskBundleCall(t, alternateMode.Tools[3], `{}`)
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

	getResult := runTaskBundleCall(t, alternateMode.Tools[2], `{"taskId":"`+created.Task.ID+`"}`)
	var fetched struct {
		Task task.Task `json:"task"`
	}
	decodeTaskBundleResult(t, getResult, &fetched)
	if got := fetched.Task.Subject; got != created.Task.Subject {
		t.Fatalf("TaskGet subject = %q, want %q", got, created.Task.Subject)
	}

	runTaskBundleCall(t, alternateMode.Tools[1], `{"taskId":"`+created.Task.ID+`","subject":"updated task","status":"in_progress"}`)
	updatedResult := runTaskBundleCall(t, alternateMode.Tools[2], `{"taskId":"`+created.Task.ID+`"}`)
	decodeTaskBundleResult(t, updatedResult, &fetched)
	if got, want := fetched.Task.Subject, "updated task"; got != want {
		t.Fatalf("TaskGet after TaskUpdate subject = %q, want %q", got, want)
	}
	if got, want := fetched.Task.Status, task.StatusInProgress; got != want {
		t.Fatalf("TaskGet after TaskUpdate status = %q, want %q", got, want)
	}

	second, err := definition.Bind(context.Background(), taskLoopBindings("44444444-4444-4444-8444-444444444444"))
	if err != nil {
		t.Fatalf("second Bind() error = %v", err)
	}
	secondMode, ok := second.Mode("")
	if !ok {
		t.Fatal("second loop base mode missing")
	}
	secondList := runTaskBundleCall(t, secondMode.Tools[3], `{}`)
	listed.Tasks = nil
	decodeTaskBundleResult(t, secondList, &listed)
	if listed.Tasks == nil {
		t.Fatal("second build TaskList returned nil tasks slice")
	}
	if got := len(listed.Tasks); got != 0 {
		t.Fatalf("second build TaskList returned %d tasks, want empty graph", got)
	}
}

func TestTaskBundleRejectsDistinctSameNameDefinitions(t *testing.T) {
	definition, err := loop.Define(loop.WithName("duplicate-task-loop"), loop.WithInference(&taskBundleInference{}, taskBundleModel()), loop.WithTools(TaskDefinitions(), TaskDefinitions()))
	if err != nil {
		t.Fatalf("loop.Define() error = %v", err)
	}
	_, err = definition.Bind(context.Background(), taskLoopBindings("55555555-5555-4555-8555-555555555555"))
	var bindErr *loop.BindError
	if !errors.As(err, &bindErr) {
		t.Fatalf("Bind() error = %T %v, want *loop.BindError", err, err)
	}
	if got, want := bindErr.Kind, loop.BindDuplicateDefinitionName; got != want {
		t.Fatalf("Bind() error kind = %q, want %q", got, want)
	}
}

func TestTaskBundleConcurrentPreparedCalls(t *testing.T) {
	definition, err := loop.Define(loop.WithName("concurrent-task-loop"), loop.WithInference(&taskBundleInference{}, taskBundleModel()), loop.WithTools(TaskDefinitions()))
	if err != nil {
		t.Fatalf("loop.Define() error = %v", err)
	}
	bound, err := definition.Bind(context.Background(), taskLoopBindings("66666666-6666-4666-8666-666666666666"))
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	mode, ok := bound.Mode("")
	if !ok {
		t.Fatal("base mode missing")
	}
	seedResult, err := invokePreparedTaskBundleCall(mode.Tools[0], `{"subject":"seed","description":"concurrency"}`)
	if err != nil {
		t.Fatalf("seed TaskCreate error = %v", err)
	}
	var seed struct {
		Task task.Task `json:"task"`
	}
	if err := decodeTaskBundleResultValue(seedResult, &seed); err != nil {
		t.Fatalf("decode seed result: %v", err)
	}

	const createCalls = 32
	const callsPerTool = 32
	var wg sync.WaitGroup
	errs := make(chan error, createCalls+callsPerTool*3)
	run := func(toolIndex int, args string) {
		defer wg.Done()
		if _, err := invokePreparedTaskBundleCall(mode.Tools[toolIndex], args); err != nil {
			errs <- err
		}
	}
	for index := 0; index < createCalls; index++ {
		wg.Add(1)
		go run(0, `{"subject":"concurrent-`+strconv.Itoa(index)+`","description":"created concurrently"}`)
	}
	for index := 0; index < callsPerTool; index++ {
		wg.Add(1)
		go run(1, `{"taskId":"`+seed.Task.ID+`","subject":"concurrent-update","status":"in_progress"}`)
		wg.Add(1)
		go run(2, `{"taskId":"`+seed.Task.ID+`"}`)
		wg.Add(1)
		go run(3, `{}`)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent prepared call error = %v", err)
	}

	finalResult, err := invokePreparedTaskBundleCall(mode.Tools[3], `{}`)
	if err != nil {
		t.Fatalf("final TaskList error = %v", err)
	}
	var final struct {
		Tasks []task.Task `json:"tasks"`
	}
	if err := decodeTaskBundleResultValue(finalResult, &final); err != nil {
		t.Fatalf("decode final TaskList result: %v", err)
	}
	if got, want := len(final.Tasks), createCalls+1; got != want {
		t.Fatalf("final TaskList returned %d tasks, want %d", got, want)
	}
	updated, err := invokePreparedTaskBundleCall(mode.Tools[2], `{"taskId":"`+seed.Task.ID+`"}`)
	if err != nil {
		t.Fatalf("final TaskGet error = %v", err)
	}
	var finalSeed struct {
		Task task.Task `json:"task"`
	}
	if err := decodeTaskBundleResultValue(updated, &finalSeed); err != nil {
		t.Fatalf("decode final TaskGet result: %v", err)
	}
	if got, want := finalSeed.Task.Subject, "concurrent-update"; got != want {
		t.Fatalf("final TaskGet subject = %q, want %q", got, want)
	}
	if got, want := finalSeed.Task.Status, task.StatusInProgress; got != want {
		t.Fatalf("final TaskGet status = %q, want %q", got, want)
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
	result, err := invokePreparedTaskBundleCall(builtTool, argsJSON)
	if err != nil {
		t.Fatalf("%T prepared call error = %v", builtTool, err)
	}
	return result
}

func invokePreparedTaskBundleCall(builtTool tool.InvokableTool, argsJSON string) (*tool.ToolResult, error) {
	preparer, ok := builtTool.(tool.CallPreparer)
	if !ok {
		return nil, fmt.Errorf("built tool %T does not implement tool.CallPreparer", builtTool)
	}
	executionID, err := uuid.New()
	if err != nil {
		return nil, fmt.Errorf("uuid.New() error = %w", err)
	}
	request, artifact, err := preparer.PrepareCall(context.Background(), executionID, argsJSON)
	if err != nil {
		return nil, fmt.Errorf("%T PrepareCall() error = %w", builtTool, err)
	}
	if artifact == nil || isNilTaskBundleArtifact(artifact) {
		return nil, fmt.Errorf("%T PrepareCall() returned nil artifact", builtTool)
	}
	ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{
		ExecutionID: executionID,
		Request:     request,
		Artifact:    artifact,
	})
	return builtTool.InvokableRun(ctx, argsJSON)
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
	if err := decodeTaskBundleResultValue(result, target); err != nil {
		t.Fatal(err)
	}
}

func decodeTaskBundleResultValue(result *tool.ToolResult, target any) error {
	if result == nil || len(result.Content) != 1 {
		return fmt.Errorf("result = %#v, want one content block", result)
	}
	block, ok := result.Content[0].(*content.TextBlock)
	if !ok {
		return fmt.Errorf("result content type = %T, want *content.TextBlock", result.Content[0])
	}
	if err := json.Unmarshal([]byte(block.Text), target); err != nil {
		return fmt.Errorf("result %q is not valid JSON: %w", block.Text, err)
	}
	return nil
}

type taskBundleInference struct{}

func (*taskBundleInference) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("unused")
}

func (*taskBundleInference) Stream(context.Context, inference.Request) (*stream.StreamReader[content.Chunk], error) {
	return nil, errors.New("unused")
}

func taskBundleModel() model.Model {
	return model.Model{Provider: model.ProviderName("lmstudio"), APIFormat: model.APIFormatOpenAI, BaseURL: "http://localhost:1234", Name: "task-bundle-test"}
}

func taskLoopBindings(loopID string) tool.Bindings {
	return tool.Bindings{SessionID: uuid.MustParse("11111111-1111-4111-8111-111111111111"), LoopID: uuid.MustParse(loopID)}
}

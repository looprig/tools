package process

// output_tool_test.go tests the ProcessOutput tool (output_tool.go) against
// the spec's "ProcessOutput API" section. It follows the same two-layer
// shape bash's own prepare/preparecall/supervised tests use: PrepareCall
// validation is tested directly (Request/artifact/error, no ctx wiring
// needed), while InvokableRun behavior is tested through the full prepared
// flow (PrepareCall -> loop.WithPreparedCall -> InvokableRun), exactly as
// the real runner drives a tool.
//
// Most fixtures here reuse existing same-package test helpers rather than
// reinventing them: newTestSupervisor/testOwner/testOrigin/testHandle
// (manifest_test.go, supervisor_test.go), registerEntry/
// waitForPendingWaiters/pendingWaiters (wait_test.go), and mustUUID
// (identity_test.go). Only the handful of helpers specific to reading
// output through a live entry -- newOutputEntry and newTerminalManifest --
// are added here.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// --- fixtures ---

// newOutputEntry builds and registers a live, non-terminal *entry for
// owner/handle directly against sup's registry, with its Spool opened
// beneath sup's OWN spoolRoot (unlike wait_test.go's newWaitEntry, which
// uses a private temp dir) -- ProcessOutputTool always reads through
// t.supervisor.resolveEntry, so the entry must live in sup's real registry
// and its Spool must be the one ProcessOutput would actually open for this
// handle. ceiling <= 0 uses the package's default (DefaultMaxProcessSpoolBytes).
func newOutputEntry(t *testing.T, sup *Supervisor, owner Owner, handle Handle, ceiling int64) *entry {
	t.Helper()
	spool, err := OpenSpool(sup.spoolRoot, handle, ceiling)
	if err != nil {
		t.Fatalf("OpenSpool() error = %v, want nil", err)
	}
	e := &entry{
		identity:  Identity{Handle: handle, Owner: owner, Origin: testOrigin(t)},
		manifests: sup.manifests,
		buffer:    NewBuffer(0),
		spool:     spool,
		done:      make(chan struct{}),
		wake:      make(chan struct{}),
	}
	registerEntry(t, sup, e)
	return e
}

// newTerminalManifest persists a full starting->running->exited manifest
// history for owner/handle directly into sup's manifest store, so a
// terminal-metadata test can assert ProcessOutput's status/exit_code/
// reason/started_at/finished_at fields without driving a real entry through
// run/terminalize.
func newTerminalManifest(t *testing.T, sup *Supervisor, owner Owner, handle Handle, exitCode int, reason string, startedAt, finishedAt time.Time) {
	t.Helper()
	id := Identity{Handle: handle, Owner: owner, Origin: testOrigin(t)}
	m := NewManifest(id, CommandMetadata{Command: "echo hi"}, AccessReadOnly, false, startedAt, nil)
	if err := sup.manifests.Save(m); err != nil {
		t.Fatalf("Save(starting) error = %v, want nil", err)
	}
	m.State = StateRunning
	m.StartedAt = &startedAt
	if err := sup.manifests.Save(m); err != nil {
		t.Fatalf("Save(running) error = %v, want nil", err)
	}
	code := exitCode
	m.State = StateExited
	m.FinishedAt = &finishedAt
	m.Result = Result{ExitCode: &code, Reason: reason}
	if err := sup.manifests.Save(m); err != nil {
		t.Fatalf("Save(exited) error = %v, want nil", err)
	}
}

// prepareOutput runs PrepareCall and fails the test on an unexpected error.
func prepareOutput(t *testing.T, tl *ProcessOutputTool, argsJSON string) (tool.Request, tool.PreparedArtifact) {
	t.Helper()
	req, art, err := tl.PrepareCall(context.Background(), mustUUID(t), argsJSON)
	if err != nil {
		t.Fatalf("PrepareCall(%s) error = %v, want nil", argsJSON, err)
	}
	return req, art
}

// prepareOutputErr runs PrepareCall and fails the test if it does NOT
// return an error.
func prepareOutputErr(t *testing.T, tl *ProcessOutputTool, argsJSON string) error {
	t.Helper()
	_, _, err := tl.PrepareCall(context.Background(), mustUUID(t), argsJSON)
	if err == nil {
		t.Fatalf("PrepareCall(%s) error = nil, want an error", argsJSON)
	}
	return err
}

// runOutputCtx drives the full prepared flow (PrepareCall, bind the
// PreparedCall to ctx, InvokableRun) and returns the single rendered text
// block, exactly as the real runner would invoke ProcessOutput.
func runOutputCtx(t *testing.T, ctx context.Context, tl *ProcessOutputTool, argsJSON string) string {
	t.Helper()
	req, art, err := tl.PrepareCall(ctx, mustUUID(t), argsJSON)
	if err != nil {
		t.Fatalf("PrepareCall(%s) error = %v, want nil", argsJSON, err)
	}
	prepCtx := loop.WithPreparedCall(ctx, tool.PreparedCall{Request: req, Artifact: art})
	result, err := tl.InvokableRun(prepCtx, argsJSON)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v, want nil (ProcessOutput never returns a Go error)", err)
	}
	return textOf(t, result)
}

func runOutput(t *testing.T, tl *ProcessOutputTool, argsJSON string) string {
	t.Helper()
	return runOutputCtx(t, context.Background(), tl, argsJSON)
}

// outputCallResult bundles one background runOutputCtxRaw call's return
// values so a test goroutine can hand them back over a channel.
type outputCallResult struct {
	text string
	err  error
}

// runOutputCtxRaw is the goroutine-safe twin of runOutputCtx: it never
// calls a *testing.T failure method (or a helper, such as mustUUID, that
// does), so a test may safely invoke it from a background goroutine --
// testing.T.FailNow's own documented contract requires FailNow (and
// therefore Fatalf) to be called only from the goroutine running the test.
// id is generated by the caller, on the test's main goroutine, precisely so
// this function never needs to call mustUUID itself. The caller performs
// its own assertions on the returned (text, err) back on the main
// goroutine.
func runOutputCtxRaw(ctx context.Context, tl *ProcessOutputTool, id uuid.UUID, argsJSON string) (string, error) {
	req, art, err := tl.PrepareCall(ctx, id, argsJSON)
	if err != nil {
		return "", err
	}
	prepCtx := loop.WithPreparedCall(ctx, tool.PreparedCall{Request: req, Artifact: art})
	result, err := tl.InvokableRun(prepCtx, argsJSON)
	if err != nil {
		return "", err
	}
	if result == nil || len(result.Content) != 1 {
		return "", fmt.Errorf("result = %+v, want exactly one content block", result)
	}
	block, ok := result.Content[0].(*content.TextBlock)
	if !ok {
		return "", fmt.Errorf("block type = %T, want *content.TextBlock", result.Content[0])
	}
	return block.Text, nil
}

func textOf(t *testing.T, result *tool.ToolResult) string {
	t.Helper()
	if result == nil || len(result.Content) != 1 {
		t.Fatalf("result = %+v, want exactly one content block", result)
	}
	block, ok := result.Content[0].(*content.TextBlock)
	if !ok {
		t.Fatalf("block type = %T, want *content.TextBlock", result.Content[0])
	}
	return block.Text
}

func decodeSingle(t *testing.T, text string) processOutputResult {
	t.Helper()
	var out processOutputResult
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v, want nil", text, err)
	}
	return out
}

func decodeMulti(t *testing.T, text string) []processOutputResult {
	t.Helper()
	var out struct {
		Results []processOutputResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v, want nil", text, err)
	}
	return out.Results
}

// --- Info ---

func TestProcessOutputInfo(t *testing.T) {
	t.Parallel()
	tl := NewProcessOutput(newTestSupervisor(t, Config{}), testOwner(t))
	info, err := tl.Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v, want nil", err)
	}
	if info.Name != "ProcessOutput" {
		t.Errorf("Info().Name = %q, want %q", info.Name, "ProcessOutput")
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(info.Schema, &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if _, ok := schema["properties"]; !ok {
		t.Error("schema has no \"properties\" key")
	}
}

// --- PrepareCall: single-vs-multi exclusivity, duplicates, empty lists ---

// TestProcessOutputPrepareCallInvalid table-drives every PrepareCall
// rejection case the task requires: single-vs-multi exclusivity (both,
// neither, empty list), duplicates, cursor, limit_bytes, wait mode, and
// encoding, plus an invalid handle shape and a negative timeout.
func TestProcessOutputPrepareCallInvalid(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, Config{})
	owner := testOwner(t)
	h1 := string(testHandle(t, 1))
	h2 := string(testHandle(t, 2))

	cases := map[string]string{
		"not json":                `not json`,
		"neither id field":        `{}`,
		"both id fields":          `{"process_id":"` + h1 + `","process_ids":["` + h1 + `"]}`,
		"empty process_ids list":  `{"process_ids":[]}`,
		"empty string in list":    `{"process_ids":["` + h1 + `",""]}`,
		"duplicate ids":           `{"process_ids":["` + h1 + `","` + h2 + `","` + h1 + `"]}`,
		"malformed single handle": `{"process_id":"not-a-valid-handle"}`,
		"malformed id in list":    `{"process_ids":["` + h1 + `","not-a-valid-handle"]}`,
		"negative cursor":         `{"process_id":"` + h1 + `","cursor":-1}`,
		"zero limit_bytes":        `{"process_id":"` + h1 + `","limit_bytes":0}`,
		"negative limit_bytes":    `{"process_id":"` + h1 + `","limit_bytes":-5}`,
		"unknown encoding":        `{"process_id":"` + h1 + `","encoding":"utf16"}`,
		"unknown wait mode":       `{"process_id":"` + h1 + `","wait":"eventually"}`,
		"negative timeout_ms":     `{"process_id":"` + h1 + `","wait":"any","timeout_ms":-1}`,
	}

	for name, argsJSON := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tl := NewProcessOutput(sup, owner)
			prepareOutputErr(t, tl, argsJSON)
		})
	}
}

// TestProcessOutputPrepareCallSingleDefaults proves a minimal single-id
// call normalizes to the documented defaults (cursor 0, limit_bytes 32768,
// encoding safe_text, wait poll, timeout_ms 0) and produces a flat
// (non-multi) artifact.
func TestProcessOutputPrepareCallSingleDefaults(t *testing.T) {
	t.Parallel()
	tl := NewProcessOutput(newTestSupervisor(t, Config{}), testOwner(t))
	h := testHandle(t, 1)

	req, artifact := prepareOutput(t, tl, `{"process_id":"`+string(h)+`"}`)
	if req.ToolName != "ProcessOutput" {
		t.Errorf("Request.ToolName = %q, want %q", req.ToolName, "ProcessOutput")
	}
	if len(req.Requirements) != 0 {
		t.Errorf("Requirements = %+v, want none (empty effect request)", req.Requirements)
	}
	if err := tool.ValidateRequest(req); err != nil {
		t.Fatalf("ValidateRequest() error = %v, want nil", err)
	}

	art, ok := artifact.(*processOutputArtifact)
	if !ok {
		t.Fatalf("artifact type = %T, want *processOutputArtifact", artifact)
	}
	if art.multi {
		t.Error("multi = true for a single process_id call, want false")
	}
	if len(art.handles) != 1 || art.handles[0] != h {
		t.Errorf("handles = %v, want [%v]", art.handles, h)
	}
	if art.cursor != 0 {
		t.Errorf("cursor = %d, want 0", art.cursor)
	}
	if art.limitBytes != int(DefaultMaxInlineResultBytes) {
		t.Errorf("limitBytes = %d, want %d", art.limitBytes, DefaultMaxInlineResultBytes)
	}
	if art.encoding != encodingSafeText {
		t.Errorf("encoding = %q, want %q", art.encoding, encodingSafeText)
	}
	if art.wait != WaitPoll {
		t.Errorf("wait = %q, want %q", art.wait, WaitPoll)
	}
	if art.timeoutMS != 0 {
		t.Errorf("timeoutMS = %d, want 0", art.timeoutMS)
	}
}

// TestProcessOutputPrepareCallMultiPreservesOrder proves a process_ids call
// produces a multi artifact preserving the caller's exact input order
// (spec: "Multi-process results preserve input order").
func TestProcessOutputPrepareCallMultiPreservesOrder(t *testing.T) {
	t.Parallel()
	tl := NewProcessOutput(newTestSupervisor(t, Config{}), testOwner(t))
	h1, h2, h3 := testHandle(t, 1), testHandle(t, 2), testHandle(t, 3)

	argsJSON := `{"process_ids":["` + string(h3) + `","` + string(h1) + `","` + string(h2) + `"],"cursor":5,"limit_bytes":10,"encoding":"base64","wait":"any","timeout_ms":250}`
	_, artifact := prepareOutput(t, tl, argsJSON)
	art, ok := artifact.(*processOutputArtifact)
	if !ok {
		t.Fatalf("artifact type = %T, want *processOutputArtifact", artifact)
	}
	if !art.multi {
		t.Error("multi = false for a process_ids call, want true")
	}
	want := []Handle{h3, h1, h2}
	if len(art.handles) != len(want) {
		t.Fatalf("handles = %v, want %v", art.handles, want)
	}
	for i := range want {
		if art.handles[i] != want[i] {
			t.Errorf("handles[%d] = %v, want %v", i, art.handles[i], want[i])
		}
	}
	if art.cursor != 5 || art.limitBytes != 10 || art.encoding != ArtifactEncodingBase64 || art.wait != WaitAny || art.timeoutMS != 250 {
		t.Errorf("parsed artifact = %+v, want cursor=5 limitBytes=10 encoding=base64 wait=any timeoutMS=250", art)
	}
}

// --- InvokableRun: poll, input-order preservation ---

// TestProcessOutputPollReadsCurrentOutput proves a poll call (the default
// wait mode) returns immediately with the process's currently retained
// output, cursors, and total_bytes -- no blocking involved.
func TestProcessOutputPollReadsCurrentOutput(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, Config{})
	owner := testOwner(t)
	h := testHandle(t, 1)
	e := newOutputEntry(t, sup, owner, h, 0)
	e.appendChunk([]byte("hello world"))

	tl := NewProcessOutput(sup, owner)
	text := runOutput(t, tl, `{"process_id":"`+string(h)+`"}`)
	got := decodeSingle(t, text)

	if got.ProcessID != string(h) {
		t.Errorf("ProcessID = %q, want %q", got.ProcessID, h)
	}
	if got.Output != "hello world" {
		t.Errorf("Output = %q, want %q", got.Output, "hello world")
	}
	if got.StartCursor != 0 || got.NextCursor != 11 || got.TotalBytes != 11 {
		t.Errorf("cursors = start=%d next=%d total=%d, want 0/11/11", got.StartCursor, got.NextCursor, got.TotalBytes)
	}
	if got.Gap {
		t.Error("Gap = true, want false (nothing dropped)")
	}
	if got.Error != "" {
		t.Errorf("Error = %q, want empty", got.Error)
	}
}

// TestProcessOutputMultiPreservesInputOrder proves the JSON results array
// for a multi-id call is ordered exactly as the caller listed process_ids,
// not sorted or otherwise reordered.
func TestProcessOutputMultiPreservesInputOrder(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, Config{})
	owner := testOwner(t)
	h1, h2, h3 := testHandle(t, 1), testHandle(t, 2), testHandle(t, 3)
	for i, h := range []Handle{h1, h2, h3} {
		e := newOutputEntry(t, sup, owner, h, 0)
		e.appendChunk([]byte{byte('A' + i)})
	}

	tl := NewProcessOutput(sup, owner)
	argsJSON := `{"process_ids":["` + string(h3) + `","` + string(h1) + `","` + string(h2) + `"]}`
	text := runOutput(t, tl, argsJSON)
	results := decodeMulti(t, text)

	want := []string{string(h3), string(h1), string(h2)}
	if len(results) != len(want) {
		t.Fatalf("len(results) = %d, want %d", len(results), len(want))
	}
	for i, id := range want {
		if results[i].ProcessID != id {
			t.Errorf("results[%d].ProcessID = %q, want %q", i, results[i].ProcessID, id)
		}
	}
}

// --- InvokableRun: gap and cursor_ahead ---

// TestProcessOutputGap proves that reading from a cursor older than the
// spool's retained window returns the earliest retained bytes with
// gap: true, rather than an error or silently reset cursor.
func TestProcessOutputGap(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, Config{})
	owner := testOwner(t)
	h := testHandle(t, 1)
	e := newOutputEntry(t, sup, owner, h, 5) // tiny spool ceiling: retains only the last 5 bytes.
	e.appendChunk([]byte("abcdefghij"))      // 10 bytes; only "fghij" (bytes 5..10) remain retained.

	tl := NewProcessOutput(sup, owner)
	text := runOutput(t, tl, `{"process_id":"`+string(h)+`","cursor":0}`)
	got := decodeSingle(t, text)

	if !got.Gap {
		t.Error("Gap = false, want true (cursor 0 is before the retained window)")
	}
	if got.Output != "fghij" {
		t.Errorf("Output = %q, want %q (gap-adjusted to the earliest retained byte)", got.Output, "fghij")
	}
	if got.StartCursor != 0 {
		t.Errorf("StartCursor = %d, want 0 (the cursor the caller supplied, before adjustment)", got.StartCursor)
	}
	if got.NextCursor != 10 || got.TotalBytes != 10 {
		t.Errorf("next_cursor/total_bytes = %d/%d, want 10/10", got.NextCursor, got.TotalBytes)
	}
}

// TestProcessOutputCursorAhead proves a cursor beyond total_bytes renders
// the stable cursor_ahead error for that entry, not a whole-call failure.
func TestProcessOutputCursorAhead(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, Config{})
	owner := testOwner(t)
	h := testHandle(t, 1)
	e := newOutputEntry(t, sup, owner, h, 0)
	e.appendChunk([]byte("hi"))

	tl := NewProcessOutput(sup, owner)
	text := runOutput(t, tl, `{"process_id":"`+string(h)+`","cursor":1000}`)
	got := decodeSingle(t, text)

	if got.Error != string(CodeCursorAhead) {
		t.Errorf("Error = %q, want %q", got.Error, CodeCursorAhead)
	}
	if got.Output != "" || got.NextCursor != 0 {
		t.Errorf("got = %+v, want a bare cursor_ahead error with no output/cursors", got)
	}
}

// --- InvokableRun: terminal metadata, opaque artifact ---

// TestProcessOutputTerminalMetadata proves the manifest-derived
// status/exit_code/reason/started_at/finished_at fields are populated for
// a process whose manifest has reached a terminal state -- read from the
// durable manifest, never computed by this file.
func TestProcessOutputTerminalMetadata(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, Config{})
	owner := testOwner(t)
	h := testHandle(t, 1)
	_ = newOutputEntry(t, sup, owner, h, 0)

	startedAt := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(250 * time.Millisecond)
	newTerminalManifest(t, sup, owner, h, 3, "exited", startedAt, finishedAt)

	tl := NewProcessOutput(sup, owner)
	text := runOutput(t, tl, `{"process_id":"`+string(h)+`"}`)
	got := decodeSingle(t, text)

	if got.Status != string(StateExited) {
		t.Errorf("Status = %q, want %q", got.Status, StateExited)
	}
	if got.ExitCode == nil || *got.ExitCode != 3 {
		t.Errorf("ExitCode = %v, want 3", got.ExitCode)
	}
	if got.Reason != "exited" {
		t.Errorf("Reason = %q, want %q", got.Reason, "exited")
	}
	if got.StartedAt != startedAt.Format(time.RFC3339Nano) {
		t.Errorf("StartedAt = %q, want %q", got.StartedAt, startedAt.Format(time.RFC3339Nano))
	}
	if got.FinishedAt != finishedAt.Format(time.RFC3339Nano) {
		t.Errorf("FinishedAt = %q, want %q", got.FinishedAt, finishedAt.Format(time.RFC3339Nano))
	}
}

// TestProcessOutputOpaqueArtifact proves the result always carries an
// artifact descriptor keyed by the process's own opaque Handle, always
// encoded base64 regardless of the READ's own requested encoding (spec:
// "independent of Binary, so a caller can retrieve the exact raw bytes ...
// even for output that rendered safely as inline text"), and that no
// filesystem path (the spool's real on-disk root) ever appears anywhere in
// the rendered JSON.
func TestProcessOutputOpaqueArtifact(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, Config{})
	owner := testOwner(t)
	h := testHandle(t, 1)
	e := newOutputEntry(t, sup, owner, h, 0)
	e.appendChunk([]byte("plain text output"))

	tl := NewProcessOutput(sup, owner)
	text := runOutput(t, tl, `{"process_id":"`+string(h)+`","encoding":"safe_text"}`)
	got := decodeSingle(t, text)

	if got.Artifact == nil {
		t.Fatal("Artifact = nil, want a populated descriptor")
	}
	if got.Artifact.ID != string(h) {
		t.Errorf("Artifact.ID = %q, want the process handle %q", got.Artifact.ID, h)
	}
	if got.Artifact.Encoding != ArtifactEncodingBase64 {
		t.Errorf("Artifact.Encoding = %q, want %q even for a safe_text read", got.Artifact.Encoding, ArtifactEncodingBase64)
	}
	if strings.Contains(text, sup.spoolRoot) {
		t.Errorf("rendered result leaks the spool's on-disk root path: %s", text)
	}
}

// --- InvokableRun: safe_text vs. base64 encoding ---

// TestProcessOutputEncodingBase64 proves base64 mode returns the exact raw
// bytes untouched by safe-text normalization -- including a raw control
// byte that safe_text mode would strip or escape.
func TestProcessOutputEncodingBase64(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, Config{})
	owner := testOwner(t)
	h := testHandle(t, 1)
	raw := []byte{'o', 'k', 0x07, 'd', 'o', 'n', 'e'} // includes a BEL control byte.
	e := newOutputEntry(t, sup, owner, h, 0)
	e.appendChunk(raw)

	tl := NewProcessOutput(sup, owner)
	text := runOutput(t, tl, `{"process_id":"`+string(h)+`","encoding":"base64"}`)
	got := decodeSingle(t, text)

	decoded, err := base64.StdEncoding.DecodeString(got.Output)
	if err != nil {
		t.Fatalf("base64.DecodeString(%q) error = %v", got.Output, err)
	}
	if string(decoded) != string(raw) {
		t.Errorf("decoded output = %q, want the exact raw bytes %q", decoded, raw)
	}
	if got.Normalized || got.Binary {
		t.Errorf("Normalized/Binary = %v/%v, want both false for a base64 read", got.Normalized, got.Binary)
	}
}

// --- InvokableRun: owner isolation and metadata-safe errors ---

// TestProcessOutputOwnerIsolation proves a missing handle and a handle
// owned by a different Owner render the IDENTICAL not_found error -- a
// caller can never distinguish "no such process" from "not yours" (spec
// "Identity and authorization").
func TestProcessOutputOwnerIsolation(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, Config{})
	owner := testOwner(t)
	other := testOwner(t)

	foreign := testHandle(t, 1)
	_ = newOutputEntry(t, sup, other, foreign, 0)
	missing := testHandle(t, 2)

	tl := NewProcessOutput(sup, owner)
	argsJSON := `{"process_ids":["` + string(foreign) + `","` + string(missing) + `"]}`
	text := runOutput(t, tl, argsJSON)
	results := decodeMulti(t, text)
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	for i, got := range results {
		if got.Error != string(CodeNotFound) {
			t.Errorf("results[%d].Error = %q, want %q", i, got.Error, CodeNotFound)
		}
		if got.Output != "" || got.Status != "" || got.Artifact != nil {
			t.Errorf("results[%d] = %+v, want a bare not_found error with no other fields", i, got)
		}
	}
}

// TestProcessOutputMetadataSafeErrors proves a not_found error never leaks
// implementation detail -- the spool's real on-disk root, or the
// underlying *Error's "process: not_found" Cause-prefixed message -- only
// the bare stable code (CLAUDE.md: "render only Code -- never Cause").
func TestProcessOutputMetadataSafeErrors(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, Config{})
	owner := testOwner(t)
	missing := testHandle(t, 1)

	tl := NewProcessOutput(sup, owner)
	text := runOutput(t, tl, `{"process_id":"`+string(missing)+`"}`)
	got := decodeSingle(t, text)

	if got.Error != "not_found" {
		t.Errorf("Error = %q, want the bare stable code %q", got.Error, "not_found")
	}
	if strings.Contains(text, "process:") {
		t.Errorf("rendered result leaks the internal error message prefix: %s", text)
	}
	if strings.Contains(text, sup.spoolRoot) {
		t.Errorf("rendered result leaks the spool's on-disk root path: %s", text)
	}
}

// TestProcessOutputInvokeWithoutPreparedCall proves InvokableRun fails
// closed with the stable invalid_arguments code when invoked outside a
// prepared call (no artifact bound to ctx) -- it never panics or falls
// through to reparsing argsJSON itself.
func TestProcessOutputInvokeWithoutPreparedCall(t *testing.T) {
	t.Parallel()
	tl := NewProcessOutput(newTestSupervisor(t, Config{}), testOwner(t))
	result, err := tl.InvokableRun(context.Background(), `{"process_id":"x"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v, want nil", err)
	}
	text := textOf(t, result)
	var out struct {
		Error string `json:"error"`
	}
	if jsonErr := json.Unmarshal([]byte(text), &out); jsonErr != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", text, jsonErr)
	}
	if out.Error != string(CodeInvalidArguments) {
		t.Errorf("Error = %q, want %q", out.Error, CodeInvalidArguments)
	}
}

// TestProcessOutputUnavailableSupervisor proves a ProcessOutputTool
// constructed without a supervisor fails every call closed, at both
// PrepareCall and InvokableRun, rather than panicking on the nil
// dependency.
func TestProcessOutputUnavailableSupervisor(t *testing.T) {
	t.Parallel()
	tl := NewProcessOutput(nil, testOwner(t))

	if _, _, err := tl.PrepareCall(context.Background(), mustUUID(t), `{"process_id":"x"}`); err == nil {
		t.Error("PrepareCall() error = nil, want an error for a nil supervisor")
	}

	result, err := tl.InvokableRun(context.Background(), `{"process_id":"x"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v, want nil", err)
	}
	text := textOf(t, result)
	if !strings.Contains(text, string(CodeLifetimeEnforcementUnavailable)) {
		t.Errorf("result = %q, want it to carry %q", text, CodeLifetimeEnforcementUnavailable)
	}
}

// --- InvokableRun: wait poll/any/all, wait timeout ---

// TestProcessOutputWaitAnyWakesOnAppend proves a wait: any call blocks
// (past its own cursor, which is set to the entry's current total_bytes so
// nothing is satisfied yet) until NEW output is appended, then returns
// promptly reflecting it -- not immediately, and not only after some
// arbitrary timeout.
func TestProcessOutputWaitAnyWakesOnAppend(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, Config{})
	owner := testOwner(t)
	h := testHandle(t, 1)
	e := newOutputEntry(t, sup, owner, h, 0)
	e.appendChunk([]byte("first"))

	tl := NewProcessOutput(sup, owner)
	argsJSON := `{"process_id":"` + string(h) + `","cursor":5,"wait":"any"}`
	id := mustUUID(t)

	resultCh := make(chan outputCallResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		text, err := runOutputCtxRaw(ctx, tl, id, argsJSON)
		resultCh <- outputCallResult{text: text, err: err}
	}()

	waitForPendingWaiters(t, sup, owner, 1)
	e.appendChunk([]byte("-second"))

	select {
	case got := <-resultCh:
		if got.err != nil {
			t.Fatalf("runOutputCtxRaw() error = %v, want nil", got.err)
		}
		result := decodeSingle(t, got.text)
		if result.Output != "-second" {
			t.Errorf("Output = %q, want %q (only the bytes past cursor 5)", result.Output, "-second")
		}
		if result.NextCursor != 12 {
			t.Errorf("NextCursor = %d, want 12", result.NextCursor)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait: any did not wake on append within 2s")
	}
}

// TestProcessOutputWaitAllRequiresEveryProcess proves a wait: all call
// blocks until EVERY selected process has advanced past its cursor, not
// just one.
func TestProcessOutputWaitAllRequiresEveryProcess(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, Config{})
	owner := testOwner(t)
	h1, h2 := testHandle(t, 1), testHandle(t, 2)
	e1 := newOutputEntry(t, sup, owner, h1, 0)
	e2 := newOutputEntry(t, sup, owner, h2, 0)

	tl := NewProcessOutput(sup, owner)
	argsJSON := `{"process_ids":["` + string(h1) + `","` + string(h2) + `"],"cursor":0,"wait":"all"}`
	id := mustUUID(t)

	resultCh := make(chan outputCallResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		text, err := runOutputCtxRaw(ctx, tl, id, argsJSON)
		resultCh <- outputCallResult{text: text, err: err}
	}()

	waitForPendingWaiters(t, sup, owner, 1)
	e1.appendChunk([]byte("only the first"))

	select {
	case got := <-resultCh:
		t.Fatalf("wait: all returned early after only one of two processes advanced: %+v", got)
	case <-time.After(150 * time.Millisecond):
		// Expected: still blocked with only one of two targets advanced.
	}

	e2.appendChunk([]byte("now the second"))

	select {
	case got := <-resultCh:
		if got.err != nil {
			t.Fatalf("runOutputCtxRaw() error = %v, want nil", got.err)
		}
		results := decodeMulti(t, got.text)
		if len(results) != 2 {
			t.Fatalf("len(results) = %d, want 2", len(results))
		}
		if results[0].Output == "" || results[1].Output == "" {
			t.Errorf("results = %+v, want both processes to report their new output", results)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait: all did not wake once every process had advanced")
	}
}

// TestProcessOutputWaitTimeoutReturnsCurrentSnapshot proves a bounded
// wait: any call that never becomes satisfied returns, once timeout_ms
// elapses, the current (unchanged) snapshot rather than an error -- "the
// wait timeout affects only the output call, never the process" (spec).
func TestProcessOutputWaitTimeoutReturnsCurrentSnapshot(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, Config{})
	owner := testOwner(t)
	h := testHandle(t, 1)
	e := newOutputEntry(t, sup, owner, h, 0)
	e.appendChunk([]byte("steady"))

	tl := NewProcessOutput(sup, owner)
	argsJSON := `{"process_id":"` + string(h) + `","cursor":6,"wait":"any","timeout_ms":20}`

	start := time.Now()
	text := runOutput(t, tl, argsJSON)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("call took %s, want it bounded near its 20ms timeout_ms", elapsed)
	}

	got := decodeSingle(t, text)
	if got.Error != "" {
		t.Errorf("Error = %q, want empty (a wait timeout is not a call failure)", got.Error)
	}
	if got.Output != "" || got.NextCursor != 6 {
		t.Errorf("got = %+v, want no new output past cursor 6", got)
	}
}

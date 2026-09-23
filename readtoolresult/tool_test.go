package readtoolresult

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

const captureID = "3f1c2b9a-7d4e-4a1b-9c8d-0e5f6a7b8c9d"

// fakeReader serves pages of one capture and records every request.
type fakeReader struct {
	mu       sync.Mutex
	data     []byte
	encoding string
	original uint64
	exact    bool
	reason   string
	requests []tool.ToolResultPageRequest
	err      error
	// pageID, when non-zero, is stamped on every page instead of the request's
	// capture id, simulating a reader that answered for another capture.
	pageID uuid.UUID
}

func (f *fakeReader) ReadToolResult(ctx context.Context, request tool.ToolResultPageRequest) (tool.ToolResultPage, error) {
	f.mu.Lock()
	f.requests = append(f.requests, request)
	f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return tool.ToolResultPage{}, err
	}
	if f.err != nil {
		return tool.ToolResultPage{}, f.err
	}
	id, err := uuid.Parse(request.CaptureID)
	if err != nil || request.CaptureID != captureID {
		return tool.ToolResultPage{}, &tool.ToolResultReadError{Kind: tool.ToolResultReadUnknownCapture}
	}
	if request.Offset >= uint64(len(f.data)) {
		return tool.ToolResultPage{}, &tool.ToolResultReadError{Kind: tool.ToolResultReadOffsetOutOfRange}
	}
	limit := request.MaxBytes
	if limit == 0 || limit > tool.MaxToolResultPageBytes {
		limit = tool.MaxToolResultPageBytes
	}
	end := min(request.Offset+limit, uint64(len(f.data)))
	if !f.pageID.IsZero() {
		id = f.pageID
	}
	return tool.ToolResultPage{
		CaptureID:        id,
		Offset:           request.Offset,
		Data:             append([]byte(nil), f.data[request.Offset:end]...),
		CapturedBytes:    uint64(len(f.data)),
		OriginalBytes:    f.original,
		OriginalExact:    f.exact,
		Truncated:        f.original > uint64(len(f.data)),
		TruncationReason: f.reason,
		Encoding:         f.encoding,
	}, nil
}

func (f *fakeReader) lastRequest(t *testing.T) tool.ToolResultPageRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("the reader was never called")
	}
	return f.requests[len(f.requests)-1]
}

func newReader(data string) *fakeReader {
	return &fakeReader{data: []byte(data), encoding: tool.ToolResultEncodingUTF8, original: uint64(len(data)), exact: true}
}

func text(t *testing.T, result *tool.ToolResult) string {
	t.Helper()
	if result == nil || len(result.Content) != 1 {
		t.Fatalf("want one content block, got %#v", result)
	}
	block, ok := result.Content[0].(*content.TextBlock)
	if !ok {
		t.Fatalf("want *content.TextBlock, got %T", result.Content[0])
	}
	return block.Text
}

// call drives the prepared flow: PrepareCall, bind the artifact, InvokableRun.
func call(t *testing.T, ctx context.Context, rt *Tool, args string) (string, error) {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	request, artifact, err := rt.PrepareCall(context.Background(), id, args)
	if err != nil {
		return "", err
	}
	if request.ToolName != loop.ReadToolResultToolName {
		t.Fatalf("request tool name = %q, want %q", request.ToolName, loop.ReadToolResultToolName)
	}
	if len(request.Requirements) != 0 {
		t.Fatalf("request requirements = %+v, want none (a read of the loop's own capture)", request.Requirements)
	}
	ctx = loop.WithPreparedCall(ctx, tool.PreparedCall{ExecutionID: id, Request: request, Artifact: artifact})
	result, err := rt.InvokableRun(ctx, args)
	if err != nil {
		t.Fatalf("InvokableRun returned a Go error %v; the tool reports failures as results", err)
	}
	return text(t, result), nil
}

func TestInfoNamesTheHarnessOwnedToolAndAStrictSchema(t *testing.T) {
	t.Parallel()
	info, err := New(newReader("x")).Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != loop.ReadToolResultToolName {
		t.Fatalf("Name = %q, want %q", info.Name, loop.ReadToolResultToolName)
	}
	var schema struct {
		Required             []string                   `json:"required"`
		AdditionalProperties *bool                      `json:"additionalProperties"`
		Properties           map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(info.Schema, &schema); err != nil {
		t.Fatalf("schema is not JSON: %v", err)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "capture_id" {
		t.Fatalf("required = %v, want [capture_id]", schema.Required)
	}
	if schema.AdditionalProperties == nil || *schema.AdditionalProperties {
		t.Fatal("schema must refuse additional properties")
	}
	for _, name := range []string{"capture_id", "offset", "max_bytes"} {
		if _, ok := schema.Properties[name]; !ok {
			t.Fatalf("schema lacks property %q", name)
		}
	}
}

func TestPagesAreRenderedByTheHarnessPage(t *testing.T) {
	t.Parallel()
	data := strings.Repeat("0123456789", 20)
	reader := newReader(data)
	rt := New(reader)

	first, err := call(t, context.Background(), rt, `{"capture_id":"`+captureID+`","max_bytes":64}`)
	if err != nil {
		t.Fatal(err)
	}
	want := tool.ToolResultPage{CaptureID: uuid.MustParse(captureID), Offset: 0, Data: []byte(data[:64]), CapturedBytes: 200, OriginalBytes: 200, OriginalExact: true, Encoding: tool.ToolResultEncodingUTF8}
	if first != want.Render() {
		t.Fatalf("first page = %q, want %q", first, want.Render())
	}
	if got := reader.lastRequest(t); got != (tool.ToolResultPageRequest{CaptureID: captureID, Offset: 0, MaxBytes: 64}) {
		t.Fatalf("request = %+v", got)
	}

	middle, err := call(t, context.Background(), rt, `{"capture_id":"`+captureID+`","offset":64,"max_bytes":64}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(middle, data[64:128]+"\n") || !strings.Contains(middle, "next_offset=128") {
		t.Fatalf("middle page = %q", middle)
	}

	last, err := call(t, context.Background(), rt, `{"capture_id":"`+captureID+`","offset":192}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(last, data[192:]+"\n") || !strings.Contains(last, "; end]") {
		t.Fatalf("final page = %q", last)
	}
	if got := reader.lastRequest(t); got.MaxBytes != 0 {
		t.Fatalf("omitted max_bytes reached the reader as %d, want 0 (the reader's own ceiling)", got.MaxBytes)
	}
}

func TestTruncatedAndBinaryCapturesRenderTheirFacts(t *testing.T) {
	t.Parallel()
	reader := &fakeReader{data: []byte{0xff, 0x00, 0xfe}, encoding: tool.ToolResultEncodingBinary, original: 10, exact: false, reason: "capture_ceiling"}
	page, err := call(t, context.Background(), New(reader), `{"capture_id":"`+captureID+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/wD+\n", "encoding=base64", "at least 10", "bytes not retained (capture_ceiling)"} {
		if !strings.Contains(page, want) {
			t.Fatalf("page %q lacks %q", page, want)
		}
	}
}

func TestPrepareRefusesMalformedArguments(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args string
	}{
		{name: "not json", args: `capture_id`},
		{name: "not an object", args: `["` + captureID + `"]`},
		{name: "empty object", args: `{}`},
		{name: "missing capture id", args: `{"offset":1}`},
		{name: "empty capture id", args: `{"capture_id":""}`},
		{name: "null capture id", args: `{"capture_id":null}`},
		{name: "capture id not a string", args: `{"capture_id":7}`},
		{name: "capture id not a uuid", args: `{"capture_id":"tool_use_1"}`},
		{name: "capture id not canonical", args: `{"capture_id":"` + strings.ToUpper(captureID) + `"}`},
		{name: "capture id with braces", args: `{"capture_id":"{` + captureID + `}"}`},
		{name: "unknown field", args: `{"capture_id":"` + captureID + `","object_id":"v1:tool-result:1:ab"}`},
		{name: "session smuggled", args: `{"capture_id":"` + captureID + `","session_id":"` + captureID + `"}`},
		{name: "duplicate capture id", args: `{"capture_id":"` + captureID + `","capture_id":"` + captureID + `"}`},
		{name: "negative offset", args: `{"capture_id":"` + captureID + `","offset":-1}`},
		{name: "fractional offset", args: `{"capture_id":"` + captureID + `","offset":1.5}`},
		{name: "string offset", args: `{"capture_id":"` + captureID + `","offset":"1"}`},
		{name: "overflowing offset", args: `{"capture_id":"` + captureID + `","offset":18446744073709551616}`},
		{name: "zero max bytes", args: `{"capture_id":"` + captureID + `","max_bytes":0}`},
		{name: "negative max bytes", args: `{"capture_id":"` + captureID + `","max_bytes":-5}`},
		{name: "trailing data", args: `{"capture_id":"` + captureID + `"} {}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			reader := newReader("data")
			_, err := call(t, context.Background(), New(reader), test.args)
			var prepareErr *PrepareError
			if !errors.As(err, &prepareErr) {
				t.Fatalf("PrepareCall(%s) error = %v, want *PrepareError", test.args, err)
			}
			if strings.Contains(err.Error(), "data") {
				t.Fatalf("error %q leaks capture content", err)
			}
			if len(reader.requests) != 0 {
				t.Fatal("a refused call reached the reader")
			}
			if strings.HasPrefix(test.name, "missing") || strings.HasPrefix(test.name, "empty capture") || strings.HasPrefix(test.name, "null capture") {
				if !strings.Contains(err.Error(), "capture_id is required") {
					t.Fatalf("error = %q, want it to say capture_id is required", err)
				}
			}
		})
	}
}

func TestPrepareAcceptsWellFormedArguments(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args string
		want tool.ToolResultPageRequest
	}{
		{name: "id only", args: `{"capture_id":"` + captureID + `"}`, want: tool.ToolResultPageRequest{CaptureID: captureID}},
		{name: "explicit zero offset", args: `{"capture_id":"` + captureID + `","offset":0}`, want: tool.ToolResultPageRequest{CaptureID: captureID}},
		{name: "null optional fields", args: `{"capture_id":"` + captureID + `","offset":null,"max_bytes":null}`, want: tool.ToolResultPageRequest{CaptureID: captureID}},
		{name: "oversized max bytes is clamped", args: `{"capture_id":"` + captureID + `","offset":3,"max_bytes":1000000}`, want: tool.ToolResultPageRequest{CaptureID: captureID, Offset: 3, MaxBytes: tool.MaxToolResultPageBytes}},
		{name: "surrounding whitespace", args: " \n{\"capture_id\":\"" + captureID + "\"}\n ", want: tool.ToolResultPageRequest{CaptureID: captureID}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			reader := newReader("0123456789")
			if _, err := call(t, context.Background(), New(reader), test.args); err != nil {
				t.Fatalf("PrepareCall error = %v", err)
			}
			if got := reader.lastRequest(t); got != test.want {
				t.Fatalf("request = %+v, want %+v", got, test.want)
			}
		})
	}
}

// TestReaderRefusalsAreModelSafeErrors proves every reader refusal — the
// unknown/foreign capture above all — is a failed result carrying only the
// harness's model-safe text, never the cause.
func TestReaderRefusalsAreModelSafeErrors(t *testing.T) {
	t.Parallel()
	secretCause := errors.New("s3://bucket/tenant-a/session-b/blobs/v1/tool-result secret")
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "unknown or foreign capture", err: &tool.ToolResultReadError{Kind: tool.ToolResultReadUnknownCapture, Cause: secretCause}, want: "error: read tool result: unknown capture_id"},
		{name: "not retained", err: &tool.ToolResultReadError{Kind: tool.ToolResultReadNotRetained}, want: "error: read tool result: the complete result is already in the conversation; nothing further was retained"},
		{name: "offset out of range", err: &tool.ToolResultReadError{Kind: tool.ToolResultReadOffsetOutOfRange}, want: "error: read tool result: offset is past the end of the retained bytes"},
		{name: "integrity", err: &tool.ToolResultReadError{Kind: tool.ToolResultReadIntegrity, Cause: secretCause}, want: "error: read tool result: the retained object failed its integrity check"},
		{name: "unavailable", err: &tool.ToolResultReadError{Kind: tool.ToolResultReadUnavailable, Cause: secretCause}, want: "error: read tool result: the retained object is unavailable"},
		{name: "wrapped typed error", err: errors.Join(errors.New("ctx"), &tool.ToolResultReadError{Kind: tool.ToolResultReadIntegrity}), want: "error: read tool result: the retained object failed its integrity check"},
		{name: "untyped error", err: secretCause, want: "error: read tool result: the retained object is unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			reader := newReader("data")
			reader.err = test.err
			got, err := call(t, context.Background(), New(reader), `{"capture_id":"`+captureID+`"}`)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("result = %q, want %q", got, test.want)
			}
		})
	}
}

func TestUnknownCaptureFromTheReaderFailsClosed(t *testing.T) {
	t.Parallel()
	got, err := call(t, context.Background(), New(newReader("data")), `{"capture_id":"00000000-0000-4000-8000-000000000000"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "error: read tool result: unknown capture_id" {
		t.Fatalf("result = %q, want the unknown-capture refusal", got)
	}
}

// TestPageForAnotherCaptureFailsClosed proves the tool never shows the model a
// page the reader served for a capture it did not ask for.
func TestPageForAnotherCaptureFailsClosed(t *testing.T) {
	t.Parallel()
	reader := newReader("secret bytes of another capture")
	reader.pageID = uuid.MustParse("99999999-9999-4999-8999-999999999999")
	got, err := call(t, context.Background(), New(reader), `{"capture_id":"`+captureID+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "error: read tool result: the retained object is unavailable" || strings.Contains(got, "secret") {
		t.Fatalf("result = %q, want a fail-closed refusal", got)
	}
}

func TestCancellationIsAFailedResult(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := call(t, ctx, New(newReader("data")), `{"capture_id":"`+captureID+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "error: read tool result: cancelled" {
		t.Fatalf("result = %q, want the cancellation refusal", got)
	}
}

func TestRunRefusesWithoutItsPreparedArtifactOrReader(t *testing.T) {
	t.Parallel()
	reader := newReader("data")
	rt := New(reader)
	for name, ctx := range map[string]context.Context{
		"no prepared call": context.Background(),
		"foreign artifact": loop.WithPreparedCall(context.Background(), tool.PreparedCall{Artifact: tool.TokenArtifact{Token: captureID}}),
	} {
		result, err := rt.InvokableRun(ctx, `{"capture_id":"`+captureID+`"}`)
		if err != nil {
			t.Fatal(err)
		}
		if got := text(t, result); got != "error: read tool result: requires its prepared call artifact" {
			t.Fatalf("%s: result = %q", name, got)
		}
	}
	if len(reader.requests) != 0 {
		t.Fatal("an unprepared call reached the reader")
	}

	var typedNil *fakeReader
	for _, nilReader := range []tool.ToolResultReader{nil, typedNil} {
		unbound := New(nilReader)
		if _, _, err := unbound.PrepareCall(context.Background(), uuid.UUID{}, `{"capture_id":"`+captureID+`"}`); err == nil {
			t.Fatal("PrepareCall succeeded without a reader")
		}
		result, err := unbound.InvokableRun(context.Background(), `{}`)
		if err != nil {
			t.Fatal(err)
		}
		if got := text(t, result); got != "error: read tool result: no retained tool results are readable in this session" {
			t.Fatalf("unbound result = %q", got)
		}
	}
}

// TestRunUsesThePreparedArgumentsNotTheRawOnes proves InvokableRun never
// re-parses its raw argument string.
func TestRunUsesThePreparedArgumentsNotTheRawOnes(t *testing.T) {
	t.Parallel()
	reader := newReader("0123456789")
	rt := New(reader)
	request, artifact, err := rt.PrepareCall(context.Background(), uuid.UUID{}, `{"capture_id":"`+captureID+`","offset":2}`)
	if err != nil {
		t.Fatal(err)
	}
	ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{Request: request, Artifact: artifact})
	if _, err := rt.InvokableRun(ctx, `{"capture_id":"00000000-0000-4000-8000-000000000000","offset":9}`); err != nil {
		t.Fatal(err)
	}
	if got := reader.lastRequest(t); got.CaptureID != captureID || got.Offset != 2 {
		t.Fatalf("request = %+v, want the prepared arguments", got)
	}
}

func TestConcurrentCallsAreIndependent(t *testing.T) {
	t.Parallel()
	data := strings.Repeat("abcdefgh", 64)
	rt := New(newReader(data))
	var group sync.WaitGroup
	for offset := 0; offset < len(data); offset += 32 {
		group.Add(1)
		go func() {
			defer group.Done()
			got, err := call(t, context.Background(), rt, `{"capture_id":"`+captureID+`","offset":`+itoa(offset)+`,"max_bytes":32}`)
			if err != nil {
				t.Error(err)
				return
			}
			if !strings.HasPrefix(got, data[offset:offset+32]+"\n") {
				t.Errorf("offset %d: page = %q", offset, got)
			}
		}()
	}
	group.Wait()
}

func itoa(n int) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

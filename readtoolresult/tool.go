// Package readtoolresult implements read_tool_result: the tool a model calls to
// page back through a tool result the loop retained in full when it was shown
// only a shaped preview.
//
// The tool is a thin, strict front end over Harness's tool.ToolResultReader.
// Harness owns everything that decides WHAT may be read: it binds one reader
// per (session, calling loop), resolves the capture id against that loop's own
// captures, verifies the stored object end to end, and fits each page under the
// loop's preview budget. The model can name only a capture id — never a
// session, tenant, object reference, backend key or path — so this package has
// nothing to authorize and no identity to resolve. What it owns is the argument
// boundary: PrepareCall refuses anything that is not exactly
// {capture_id, offset?, max_bytes?}, and a reader failure is reported as the
// reader's own model-safe text, never its cause.
package readtoolresult

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/internal/workspace"
)

// schema is the model-facing argument contract. PrepareCall enforces the same
// rules independently; the schema only describes them.
const schema = `{
  "type": "object",
  "properties": {
    "capture_id": {"type": "string", "description": "The capture id named by a retention marker: read_tool_result capture_id=\"<uuid>\"."},
    "offset": {"type": "integer", "minimum": 0, "description": "First retained byte to return (optional; default 0). Use the next_offset a previous page reported."},
    "max_bytes": {"type": "integer", "minimum": 1, "description": "Maximum retained bytes to return (optional; default and ceiling 65536, lowered to fit the conversation's tool-result budget)."}
  },
  "required": ["capture_id"],
  "additionalProperties": false
}`

const description = "Read a retained tool result that was shown only in part. Pass the capture_id from the marker '[tool output shaped; ... read the rest with read_tool_result capture_id=\"<uuid>\"]' and page forward with the next_offset each page reports. Each page ends with a footer giving its byte range; binary results are returned base64-encoded. Only this conversation's own retained results can be read."

// Tool is the read_tool_result tool bound to one loop's reader.
type Tool struct {
	reader tool.ToolResultReader
}

// New binds the tool to reader, the loop-scoped reader Harness supplies in
// tool.Bindings.ToolResults. A nil or typed-nil reader yields a tool that
// refuses every call.
func New(reader tool.ToolResultReader) *Tool {
	if workspace.IsNil(reader) {
		reader = nil
	}
	return &Tool{reader: reader}
}

// Info returns the tool's self-description. The name is Harness's constant, so
// the retention marker and the tool cannot drift apart.
func (t *Tool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   loop.ReadToolResultToolName,
		Desc:   description,
		Schema: json.RawMessage(schema),
	}, nil
}

// PrepareError is a refused argument document. Its message is model-safe.
type PrepareError struct{ reason string }

func (e *PrepareError) Error() string { return "read_tool_result: " + e.reason }

func refuse(format string, args ...any) error {
	return &PrepareError{reason: fmt.Sprintf(format, args...)}
}

// artifact is the sealed, validated request InvokableRun executes.
type artifact struct {
	tool.TokenArtifact
	request tool.ToolResultPageRequest
}

// arguments is the typed decode. Pointers distinguish an omitted (or null)
// optional field from an explicit value.
type arguments struct {
	CaptureID *string `json:"capture_id"`
	Offset    *uint64 `json:"offset"`
	MaxBytes  *uint64 `json:"max_bytes"`
}

// PrepareCall decodes and validates the arguments once and freezes them. It
// refuses: anything but exactly one JSON object; unknown or duplicated members;
// a capture_id that is not a canonical lower-case UUID (the only form the
// marker prints); an offset or max_bytes that is not a non-negative integer;
// and max_bytes of zero. max_bytes above the page ceiling is clamped to it.
//
// The request carries no requirements: a loop reading its own retained output
// performs no new effect, and the reader Harness bound already confines it to
// that loop's captures.
func (t *Tool) PrepareCall(_ context.Context, _ uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	if t.reader == nil {
		return tool.Request{}, nil, refuse("no retained tool results are readable in this session")
	}
	args, err := decodeArguments(argsJSON)
	if err != nil {
		return tool.Request{}, nil, err
	}
	if args.CaptureID == nil || *args.CaptureID == "" {
		return tool.Request{}, nil, refuse("capture_id is required")
	}
	parsed, err := uuid.Parse(*args.CaptureID)
	if err != nil || parsed.String() != *args.CaptureID {
		return tool.Request{}, nil, refuse("capture_id must be the canonical id from a retention marker")
	}
	request := tool.ToolResultPageRequest{CaptureID: *args.CaptureID}
	if args.Offset != nil {
		request.Offset = *args.Offset
	}
	if args.MaxBytes != nil {
		if *args.MaxBytes == 0 {
			return tool.Request{}, nil, refuse("max_bytes must be at least 1")
		}
		request.MaxBytes = min(*args.MaxBytes, tool.MaxToolResultPageBytes)
	}
	return tool.Request{ToolName: loop.ReadToolResultToolName}, &artifact{request: request}, nil
}

// decodeArguments strictly decodes one JSON object.
func decodeArguments(argsJSON string) (arguments, error) {
	raw := []byte(argsJSON)
	if err := refuseDuplicateMembers(raw); err != nil {
		return arguments{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var args arguments
	if err := decoder.Decode(&args); err != nil {
		return arguments{}, refuse("arguments must be one JSON object with capture_id and optional non-negative integer offset and max_bytes")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return arguments{}, refuse("arguments must be exactly one JSON object")
	}
	return args, nil
}

// refuseDuplicateMembers rejects a top-level object that names a member twice.
// encoding/json would silently keep the last one, so the value the model meant
// would be ambiguous. Anything that is not an object is left for the typed
// decode to refuse.
func refuseDuplicateMembers(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil
	}
	seen := make(map[string]bool)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil
		}
		name, ok := token.(string)
		if !ok {
			return nil
		}
		if seen[name] {
			return refuse("argument %q is given more than once", name)
		}
		seen[name] = true
		var skip json.RawMessage
		if err := decoder.Decode(&skip); err != nil {
			return nil
		}
	}
	return nil
}

// InvokableRun reads the prepared page. It never re-parses argsJSON and never
// returns a Go error: every failure is a result the model can act on, carrying
// only model-safe text.
func (t *Tool) InvokableRun(ctx context.Context, _ string) (*tool.ToolResult, error) {
	if t.reader == nil {
		return failure("no retained tool results are readable in this session"), nil
	}
	call, ok := loop.PreparedCallFromContext(ctx)
	if !ok {
		return failure("requires its prepared call artifact"), nil
	}
	prepared, ok := call.Artifact.(*artifact)
	if !ok || prepared == nil {
		return failure("requires its prepared call artifact"), nil
	}
	page, err := t.reader.ReadToolResult(ctx, prepared.request)
	if err != nil {
		return readFailure(ctx, err), nil
	}
	// A page for any capture but the requested one is never shown: the reader
	// answered a question the model did not ask.
	if page.CaptureID.String() != prepared.request.CaptureID {
		return failure("the retained object is unavailable"), nil
	}
	return tool.TextResult(page.Render()), nil
}

// readFailure maps a reader error onto model-visible text. A typed reader error
// renders its own model-safe message; cancellation says so; anything else is
// reported as unavailable, because an untyped error may carry a backend detail.
func readFailure(ctx context.Context, err error) *tool.ToolResult {
	var readErr *tool.ToolResultReadError
	switch {
	case errors.As(err, &readErr):
		return tool.TextResult("error: " + readErr.Error())
	case ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		return failure("cancelled")
	default:
		return failure("the retained object is unavailable")
	}
}

func failure(reason string) *tool.ToolResult {
	return tool.TextResult("error: read tool result: " + reason)
}

var (
	_ tool.InvokableTool = (*Tool)(nil)
	_ tool.CallPreparer  = (*Tool)(nil)
)

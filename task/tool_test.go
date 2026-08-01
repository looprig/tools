package task

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/tool"
)

type strictDecodeArgs struct {
	Subject string `json:"subject"`
	Count   int    `json:"count"`
	Enabled bool   `json:"enabled"`
}

type decodeProbe struct {
	calls *int
}

func (p *decodeProbe) UnmarshalJSON([]byte) error {
	*p.calls++
	return nil
}

func TestDecodeRejectsOversizedRawDocumentBeforeDecode(t *testing.T) {
	calls := 0
	probe := &decodeProbe{calls: &calls}
	raw := `{"subject":"` + strings.Repeat("x", maxTaskArgsBytes) + `"}`

	if _, err := decodeObject(raw, probe); err == nil {
		t.Fatal("decodeObject accepted an oversized raw document")
	}
	if calls != 0 {
		t.Fatalf("target decoder was called %d times for an oversized raw document", calls)
	}
}

func TestDecodeRejectsMalformedTrailingNonObjectAndUnknownJSON(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "malformed", raw: `{"subject":`},
		{name: "trailing", raw: `{"subject":"ok"} {}`},
		{name: "null", raw: `null`},
		{name: "array", raw: `[]`},
		{name: "scalar string", raw: `"subject"`},
		{name: "scalar number", raw: `0`},
		{name: "unknown field", raw: `{"subject":"ok","extra":true}`},
		{name: "case variant field", raw: `{"Subject":"x"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := decodeObject(tt.raw, &strictDecodeArgs{}); err == nil {
				t.Fatalf("decodeObject accepted %s JSON: %s", tt.name, tt.raw)
			}
		})
	}
}

func TestDecodeRejectsDuplicateTopLevelObjectMembers(t *testing.T) {
	if _, err := decodeObject(`{"subject":"first","subject":"second"}`, &strictDecodeArgs{}); err == nil {
		t.Fatal("decodeObject accepted duplicate top-level object members")
	}
}

func TestDecodeTracksFieldPresenceIndependentlyOfZeroValues(t *testing.T) {
	var omitted strictDecodeArgs
	omittedFields, err := decodeObject(`{}`, &omitted)
	if err != nil {
		t.Fatalf("decodeObject(omitted) error = %v", err)
	}
	if omitted.Count != 0 || omitted.Enabled {
		t.Fatalf("omitted zero fields decoded as %#v", omitted)
	}
	if omittedFields.has("count") || omittedFields.has("enabled") {
		t.Fatalf("omitted fields reported present: %#v", omittedFields)
	}

	var supplied strictDecodeArgs
	suppliedFields, err := decodeObject(`{"count":0,"enabled":false}`, &supplied)
	if err != nil {
		t.Fatalf("decodeObject(supplied) error = %v", err)
	}
	if supplied.Count != 0 || supplied.Enabled {
		t.Fatalf("supplied zero fields decoded as %#v", supplied)
	}
	if !suppliedFields.has("count") || !suppliedFields.has("enabled") {
		t.Fatalf("supplied zero fields not reported present: %#v", suppliedFields)
	}
}

func TestMetadataCanonicalizesKeysAndOwnsBytes(t *testing.T) {
	raw := json.RawMessage(` { "z": {"b": 2, "a": 1}, "a": 1 } `)
	want := `{"a":1,"z":{"a":1,"b":2}}`

	got, err := canonicalMetadata(raw)
	if err != nil {
		t.Fatalf("canonicalMetadata() error = %v", err)
	}
	if string(got) != want {
		t.Fatalf("canonicalMetadata() = %s, want %s", got, want)
	}

	raw[1] = '['
	if string(got) != want {
		t.Fatalf("mutating input metadata changed result: %s", got)
	}
	got[0] = '['
	if string(raw) == string(got) {
		t.Fatalf("mutating result metadata changed input backing storage: %s", raw)
	}
}

func TestMetadataRejectsMalformedTrailingOversizedAndNonObjectValues(t *testing.T) {
	overSized := `{"value":"` + strings.Repeat("x", maxMetadataBytes) + `"}`
	tests := []struct {
		name string
		raw  json.RawMessage
	}{
		{name: "malformed", raw: json.RawMessage(`{"key":`)},
		{name: "trailing", raw: json.RawMessage(`{} {}`)},
		{name: "null", raw: json.RawMessage(`null`)},
		{name: "array", raw: json.RawMessage(`[]`)},
		{name: "scalar", raw: json.RawMessage(`"value"`)},
		{name: "oversized", raw: json.RawMessage(overSized)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := canonicalMetadata(tt.raw); err == nil {
				t.Fatalf("canonicalMetadata() = %s, want error", got)
			}
		})
	}
}

func TestMetadataOmissionStaysNilAndEmptyObjectIsClearSentinel(t *testing.T) {
	omitted, err := canonicalMetadata(nil)
	if err != nil {
		t.Fatalf("canonicalMetadata(nil) error = %v", err)
	}
	if omitted != nil {
		t.Fatalf("omitted metadata = %s, want nil", omitted)
	}

	clear, err := canonicalMetadata(json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("canonicalMetadata({}) error = %v", err)
	}
	if clear == nil || string(clear) != `{}` {
		t.Fatalf("empty metadata = %q, want non-nil {} clear sentinel", clear)
	}
}

func TestJSONResultReturnsOneValidTextBlock(t *testing.T) {
	result, err := jsonResult(struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}{Name: "task", Count: 0})
	if err != nil {
		t.Fatalf("jsonResult() error = %v", err)
	}
	if result == nil {
		t.Fatal("jsonResult() returned nil result")
	}
	if len(result.Content) != 1 {
		t.Fatalf("jsonResult() returned %d content blocks, want 1", len(result.Content))
	}
	block, ok := result.Content[0].(*content.TextBlock)
	if !ok {
		t.Fatalf("jsonResult() block type = %T, want *content.TextBlock", result.Content[0])
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(block.Text), &decoded); err != nil {
		t.Fatalf("jsonResult() text is not valid JSON: %v", err)
	}
	if decoded["name"] != "task" || decoded["count"] != float64(0) {
		t.Fatalf("jsonResult() text decoded as %#v", decoded)
	}
}

func TestPrepareErrorHasStableModelSafeText(t *testing.T) {
	const secret = "top-secret task text and metadata"
	_, err := decodeObject(`{"subject":"`+secret+`","unexpected":true}`, &strictDecodeArgs{})
	if err == nil {
		t.Fatal("decodeObject() succeeded for unknown field")
	}
	var typed *prepareError
	if !errors.As(err, &typed) {
		t.Fatalf("error type = %T, want *prepareError", err)
	}
	if got, want := err.Error(), "invalid task arguments"; got != want {
		t.Fatalf("prepareError.Error() = %q, want %q", got, want)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("prepareError leaked secret text: %q", err)
	}
}

func TestAuditSummaryIsExactlyConcreteToolName(t *testing.T) {
	base := toolBase{name: "TaskCreate", desc: "private description"}
	if got, want := base.AuditSummary(`{"subject":"secret"}`), "TaskCreate"; got != want {
		t.Fatalf("AuditSummary() = %q, want %q", got, want)
	}
	var _ tool.Auditable = base
	if _, implementsBaseTool := any(base).(tool.BaseTool); implementsBaseTool {
		t.Fatal("toolBase unexpectedly implements tool.BaseTool")
	}
}

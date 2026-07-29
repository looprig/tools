package process

import (
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// fakeReader is a directly-constructed Reader implementation (per the task
// doc: "tests constructing those inputs directly rather than through a full
// supervisor") for scenarios a real Buffer/Spool can't conveniently
// produce, such as an injected error.
type fakeReader struct {
	data       []byte
	nextCursor int64
	gap        bool
	err        error
}

func (f fakeReader) Read(cursor int64, maxBytes int) ([]byte, int64, bool, error) {
	if f.err != nil {
		return nil, f.nextCursor, false, f.err
	}
	data := f.data
	if maxBytes > 0 && len(data) > maxBytes {
		data = data[:maxBytes]
	}
	return data, f.nextCursor, f.gap, nil
}

// --- RenderSafeText: normalization and the "normalized" flag ---

func TestRenderSafeTextNormalizesAndReportsChanged(t *testing.T) {
	t.Parallel()
	h := testHandle(t, 1)
	r := fakeReader{data: []byte("clean\x1b[31mred\x1b[0mtext\x00tail"), nextCursor: 30}

	got, err := RenderSafeText(r, h, 0, 0, 0)
	if err != nil {
		t.Fatalf("RenderSafeText() err = %v", err)
	}
	want := "cleanredtexttail"
	if got.Output != want {
		t.Errorf("Output = %q, want %q", got.Output, want)
	}
	if !got.Normalized {
		t.Errorf("Normalized = false, want true: control/escape sequences were removed")
	}
}

func TestRenderSafeTextUnchangedReportsNotNormalized(t *testing.T) {
	t.Parallel()
	h := testHandle(t, 1)
	raw := []byte("ordinary safe text, unicode héllo, tabs\tand\nnewlines")
	r := fakeReader{data: raw, nextCursor: int64(len(raw))}

	got, err := RenderSafeText(r, h, 0, 0, 0)
	if err != nil {
		t.Fatalf("RenderSafeText() err = %v", err)
	}
	if got.Output != string(raw) {
		t.Errorf("Output = %q, want unchanged %q", got.Output, raw)
	}
	if got.Normalized {
		t.Errorf("Normalized = true, want false: input was already safe text")
	}
}

// --- RenderSafeText: binary detection propagates ---

func TestRenderSafeTextReportsBinaryForNULHeavyData(t *testing.T) {
	t.Parallel()
	h := testHandle(t, 1)
	data := make([]byte, 40)
	for i := range data {
		if i%2 == 0 {
			data[i] = 0
		} else {
			data[i] = 'x'
		}
	}
	r := fakeReader{data: data, nextCursor: int64(len(data))}

	got, err := RenderSafeText(r, h, 0, 0, 0)
	if err != nil {
		t.Fatalf("RenderSafeText() err = %v", err)
	}
	if !got.Binary {
		t.Errorf("Binary = false, want true for NUL-heavy data")
	}
}

func TestRenderSafeTextReportsNotBinaryForOrdinaryText(t *testing.T) {
	t.Parallel()
	h := testHandle(t, 1)
	raw := []byte(strings.Repeat("ordinary log line\n", 20))
	r := fakeReader{data: raw, nextCursor: int64(len(raw))}

	got, err := RenderSafeText(r, h, 0, 0, 0)
	if err != nil {
		t.Fatalf("RenderSafeText() err = %v", err)
	}
	if got.Binary {
		t.Errorf("Binary = true, want false for ordinary text")
	}
}

// --- RenderSafeText: capping never splits a replacement sequence ---

func TestRenderSafeTextCapsOutputWithoutSplittingReplacement(t *testing.T) {
	t.Parallel()
	h := testHandle(t, 1)
	// Invalid bytes scattered through the input so several candidate cap
	// sizes land in the middle of a 3-byte U+FFFD replacement sequence.
	raw := []byte(strings.Repeat("ab\xFFcd\xFFef", 20))
	r := fakeReader{data: raw, nextCursor: int64(len(raw))}

	for limit := 1; limit <= 40; limit++ {
		got, err := RenderSafeText(r, h, 0, 0, int64(limit))
		if err != nil {
			t.Fatalf("RenderSafeText(cap=%d) err = %v", limit, err)
		}
		if len(got.Output) > limit {
			t.Errorf("cap=%d: len(Output) = %d, exceeds cap", limit, len(got.Output))
		}
		if !utf8.ValidString(got.Output) {
			t.Errorf("cap=%d: Output = %q is not valid UTF-8 (split a replacement sequence)", limit, got.Output)
		}
	}
}

func TestRenderSafeTextDefaultsCapBytes(t *testing.T) {
	t.Parallel()
	h := testHandle(t, 1)
	raw := make([]byte, DefaultMaxInlineResultBytes+1000)
	for i := range raw {
		raw[i] = 'a'
	}
	r := fakeReader{data: raw, nextCursor: int64(len(raw))}

	got, err := RenderSafeText(r, h, 0, 0, 0) // capBytes 0 -> default
	if err != nil {
		t.Fatalf("RenderSafeText() err = %v", err)
	}
	if int64(len(got.Output)) != DefaultMaxInlineResultBytes {
		t.Errorf("len(Output) = %d, want default cap %d", len(got.Output), DefaultMaxInlineResultBytes)
	}
}

// --- RenderSafeText: gap and cursor-ahead propagate from Reader ---

func TestRenderSafeTextPropagatesGap(t *testing.T) {
	t.Parallel()
	h := testHandle(t, 1)
	b := NewBuffer(5)
	if _, err := b.Append([]byte("0123456789")); err != nil { // capacity 5, total 10: cursor 0 is a gap
		t.Fatalf("Append() err = %v", err)
	}
	got, err := RenderSafeText(b, h, 0, 0, 0)
	if err != nil {
		t.Fatalf("RenderSafeText() err = %v", err)
	}
	if !got.Gap {
		t.Errorf("Gap = false, want true")
	}
	if got.Output != "56789" {
		t.Errorf("Output = %q, want %q (earliest retained bytes)", got.Output, "56789")
	}
	if got.NextCursor != 10 {
		t.Errorf("NextCursor = %d, want 10", got.NextCursor)
	}
}

func TestRenderSafeTextPropagatesCursorAheadError(t *testing.T) {
	t.Parallel()
	h := testHandle(t, 1)
	r := fakeReader{err: New(CodeCursorAhead), nextCursor: 3}

	_, err := RenderSafeText(r, h, 1000, 10, 0)
	if !errors.Is(err, New(CodeCursorAhead)) {
		t.Errorf("RenderSafeText() err = %v, want CodeCursorAhead", err)
	}
}

// --- Artifact: opaque, no path, always populated ---

func TestRenderSafeTextArtifactReferencesHandleAndCursorRangeOnly(t *testing.T) {
	t.Parallel()
	h := testHandle(t, 7)
	r := fakeReader{data: []byte("hello"), nextCursor: 42}

	got, err := RenderSafeText(r, h, 5, 0, 0)
	if err != nil {
		t.Fatalf("RenderSafeText() err = %v", err)
	}
	want := Artifact{ProcessID: h, StartCursor: 5, EndCursor: 42, Encoding: ArtifactEncodingBase64}
	if got.Artifact != want {
		t.Errorf("Artifact = %+v, want %+v", got.Artifact, want)
	}
}

// TestArtifactStructCarriesNoPathOrHostDetail proves, structurally, that
// Artifact cannot leak a filesystem path or other host detail: it walks the
// type's exported fields by reflection and fails if any field's name (or,
// for string-typed fields, its rendered value for a representative
// Artifact) mentions a path/directory/file concept. This guards against a
// future edit silently adding a path field to Artifact, not just against
// today's implementation.
func TestArtifactStructCarriesNoPathOrHostDetail(t *testing.T) {
	t.Parallel()
	h := testHandle(t, 1)
	a := NewArtifact(h, 0, 100)

	typ := reflect.TypeOf(a)
	suspicious := []string{"path", "dir", "file", "root", "pid"}
	for i := 0; i < typ.NumField(); i++ {
		name := strings.ToLower(typ.Field(i).Name)
		for _, s := range suspicious {
			if strings.Contains(name, s) {
				t.Errorf("Artifact field %q looks like it might carry host detail (matched %q)", typ.Field(i).Name, s)
			}
		}
	}

	if strings.ContainsAny(string(a.ProcessID), "/\\") {
		t.Errorf("Artifact.ProcessID = %q contains a path separator", a.ProcessID)
	}
	if a.Encoding != ArtifactEncodingBase64 {
		t.Errorf("Artifact.Encoding = %q, want %q", a.Encoding, ArtifactEncodingBase64)
	}
}

// --- RenderBase64: exact raw bytes, no normalization ---

func TestRenderBase64ReturnsExactRawBytes(t *testing.T) {
	t.Parallel()
	// Deliberately includes invalid UTF-8, C0 controls, and an ANSI escape
	// sequence -- everything RenderSafeText would strip or replace -- to
	// prove base64 mode passes bytes through completely untouched.
	raw := []byte{'a', 0x1B, '[', '3', '1', 'm', 0x00, 0xFF, 'z'}
	r := fakeReader{data: raw, nextCursor: int64(len(raw))}

	got, err := RenderBase64(r, 0, 0)
	if err != nil {
		t.Fatalf("RenderBase64() err = %v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(got.Data)
	if err != nil {
		t.Fatalf("base64 decode err = %v", err)
	}
	if string(decoded) != string(raw) {
		t.Errorf("decoded base64 = %v, want exact raw bytes %v", decoded, raw)
	}
}

func TestRenderBase64PropagatesGapAndCursors(t *testing.T) {
	t.Parallel()
	b := NewBuffer(5)
	if _, err := b.Append([]byte("0123456789")); err != nil {
		t.Fatalf("Append() err = %v", err)
	}
	got, err := RenderBase64(b, 0, 0)
	if err != nil {
		t.Fatalf("RenderBase64() err = %v", err)
	}
	if !got.Gap {
		t.Errorf("Gap = false, want true")
	}
	if got.StartCursor != 0 || got.NextCursor != 10 {
		t.Errorf("StartCursor/NextCursor = %d/%d, want 0/10", got.StartCursor, got.NextCursor)
	}
	decoded, err := base64.StdEncoding.DecodeString(got.Data)
	if err != nil {
		t.Fatalf("base64 decode err = %v", err)
	}
	if string(decoded) != "56789" {
		t.Errorf("decoded = %q, want %q", decoded, "56789")
	}
}

func TestRenderBase64PropagatesCursorAheadError(t *testing.T) {
	t.Parallel()
	r := fakeReader{err: New(CodeCursorAhead), nextCursor: 3}

	_, err := RenderBase64(r, 1000, 10)
	if !errors.Is(err, New(CodeCursorAhead)) {
		t.Errorf("RenderBase64() err = %v, want CodeCursorAhead", err)
	}
}

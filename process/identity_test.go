package process

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
)

// TestHandleEntropyAtLeast128Bits verifies every generated Handle decodes to
// exactly HandleEntropyBytes (16 bytes = 128 bits) of URL-safe base64, the
// spec's documented minimum ("Output capture and storage": "Process handle
// entropy | at least 128 bits").
func TestHandleEntropyAtLeast128Bits(t *testing.T) {
	t.Parallel()
	const iterations = 200
	seen := make(map[Handle]bool, iterations)
	for i := 0; i < iterations; i++ {
		h, err := NewHandle(nil)
		if err != nil {
			t.Fatalf("NewHandle() iteration %d err = %v", i, err)
		}
		decoded, err := base64.RawURLEncoding.DecodeString(string(h))
		if err != nil {
			t.Fatalf("iteration %d: handle %q is not valid unpadded URL-safe base64: %v", i, h, err)
		}
		if len(decoded) != HandleEntropyBytes {
			t.Fatalf("iteration %d: decoded handle length = %d bytes, want %d (>= 128 bits)", i, len(decoded), HandleEntropyBytes)
		}
		if !h.Valid() {
			t.Errorf("iteration %d: Handle(%q).Valid() = false, want true", i, h)
		}
		// URL-safe: must never contain '+' or '/' (the non-URL-safe base64
		// alphabet members), and must be unpadded (no '=').
		if strings.ContainsAny(string(h), "+/=") {
			t.Errorf("iteration %d: handle %q is not URL-safe/unpadded", i, h)
		}
		if seen[h] {
			t.Errorf("iteration %d: NewHandle() returned duplicate %q", i, h)
		}
		seen[h] = true
	}
}

// TestHandleContainsNoOwnerPathTimestampOrPID proves a Handle's decoded bytes
// are exactly the injected randomness, verbatim — nothing else (no owner,
// path, timestamp, or OS PID) is mixed in. generateHandle's own signature is
// the structural half of this proof: it accepts only an io.Reader and a
// HandleExists check, so there is no Owner, Origin, path, clock, or PID input
// for it to encode in the first place.
func TestHandleContainsNoOwnerPathTimestampOrPID(t *testing.T) {
	t.Parallel()
	seed := make([]byte, HandleEntropyBytes)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	h, err := generateHandle(bytes.NewReader(seed), nil)
	if err != nil {
		t.Fatalf("generateHandle() err = %v", err)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(string(h))
	if err != nil {
		t.Fatalf("decode handle: %v", err)
	}
	if !bytes.Equal(decoded, seed) {
		t.Errorf("decoded handle bytes = %v, want exactly the injected randomness %v (no owner/path/timestamp/PID mixed in)", decoded, seed)
	}
}

// errReader is a reader that always fails, used to drive the GenerateError
// path (mirrors core/uuid's errReader test seam).
type errReader struct{ err error }

func (r errReader) Read(_ []byte) (int, error) { return 0, r.err }

// TestGenerateHandleGeneratorFailureIsTyped verifies a randomness-source
// failure (including a short read) surfaces as a *GenerateError that unwraps
// to the underlying cause.
func TestGenerateHandleGeneratorFailureIsTyped(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom")
	tests := []struct {
		name    string
		reader  io.Reader
		wantErr error
	}{
		{name: "explicit reader error", reader: errReader{err: sentinel}, wantErr: sentinel},
		{name: "short read", reader: bytes.NewReader([]byte{0x00, 0x01}), wantErr: io.ErrUnexpectedEOF},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h, err := generateHandle(tt.reader, nil)
			if err == nil {
				t.Fatalf("generateHandle() err = nil, want error")
			}
			if h != "" {
				t.Errorf("generateHandle() = %q on error, want empty", h)
			}
			var genErr *GenerateError
			if !errors.As(err, &genErr) {
				t.Fatalf("generateHandle() err = %v, want *GenerateError", err)
			}
			if !errors.Is(genErr.Err, tt.wantErr) {
				t.Errorf("GenerateError.Err = %v, want %v", genErr.Err, tt.wantErr)
			}
			if !errors.Is(errors.Unwrap(err), tt.wantErr) {
				t.Errorf("errors.Unwrap(err) = %v, want %v", errors.Unwrap(err), tt.wantErr)
			}
		})
	}
}

// repeatingReader deterministically cycles through a fixed byte sequence so
// tests can control exactly which candidate bytes generateHandle sees on
// each attempt without depending on crypto/rand.
type repeatingReader struct {
	data []byte
	pos  int
}

func (r *repeatingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.data[r.pos%len(r.data)]
		r.pos++
	}
	return len(p), nil
}

// TestGenerateHandleCollisionRetryIsTyped verifies that when every candidate
// collides against the supplied HandleExists check, generateHandle retries up
// to maxGenerateAttempts times and then returns a typed *CollisionError
// rather than looping forever.
func TestGenerateHandleCollisionRetryIsTyped(t *testing.T) {
	t.Parallel()
	r := &repeatingReader{data: []byte{0xAB}}
	alwaysExists := func(Handle) bool { return true }

	h, err := generateHandle(r, alwaysExists)
	if err == nil {
		t.Fatalf("generateHandle() err = nil, want *CollisionError")
	}
	if h != "" {
		t.Errorf("generateHandle() = %q on collision exhaustion, want empty", h)
	}
	var collErr *CollisionError
	if !errors.As(err, &collErr) {
		t.Fatalf("generateHandle() err = %v, want *CollisionError", err)
	}
	if collErr.Attempts != maxGenerateAttempts {
		t.Errorf("CollisionError.Attempts = %d, want %d", collErr.Attempts, maxGenerateAttempts)
	}
}

// TestGenerateHandleCollisionRetrySucceeds verifies generateHandle retries
// past a colliding candidate and succeeds once HandleExists reports a
// candidate as unused, without exhausting the retry budget.
func TestGenerateHandleCollisionRetrySucceeds(t *testing.T) {
	t.Parallel()
	// Two distinct 16-byte candidates: the reader yields the first candidate
	// bytes, then the second, then repeats the second forever.
	first := bytes.Repeat([]byte{0x01}, HandleEntropyBytes)
	second := bytes.Repeat([]byte{0x02}, HandleEntropyBytes)
	r := &repeatingReader{data: append(append([]byte{}, first...), second...)}

	firstCandidate := Handle(base64.RawURLEncoding.EncodeToString(first))
	calls := 0
	existsOnlyFirst := func(h Handle) bool {
		calls++
		return h == firstCandidate
	}

	h, err := generateHandle(r, existsOnlyFirst)
	if err != nil {
		t.Fatalf("generateHandle() err = %v, want nil", err)
	}
	if h == firstCandidate {
		t.Errorf("generateHandle() = %q, want retry past the colliding first candidate", h)
	}
	if !h.Valid() {
		t.Errorf("generateHandle() = %q is not a well-formed handle", h)
	}
	if calls < 2 {
		t.Errorf("HandleExists called %d times, want at least 2 (collision then retry)", calls)
	}
}

// TestHandleValid table-tests Handle.Valid against well-formed and malformed
// encodings.
func TestHandleValid(t *testing.T) {
	t.Parallel()
	wellFormed, err := NewHandle(nil)
	if err != nil {
		t.Fatalf("NewHandle() err = %v", err)
	}
	tests := []struct {
		name string
		h    Handle
		want bool
	}{
		{name: "well-formed generated handle", h: wellFormed, want: true},
		{name: "empty", h: "", want: false},
		{name: "too short", h: Handle(base64.RawURLEncoding.EncodeToString([]byte("short"))), want: false},
		{name: "too long", h: Handle(base64.RawURLEncoding.EncodeToString(make([]byte, HandleEntropyBytes+1))), want: false},
		{name: "padded base64 rejected", h: Handle(base64.URLEncoding.EncodeToString(make([]byte, HandleEntropyBytes))), want: false},
		{name: "not base64 at all", h: "not a handle!!", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.h.Valid(); got != tt.want {
				t.Errorf("Handle(%q).Valid() = %v, want %v", tt.h, got, tt.want)
			}
		})
	}
}

func mustUUID(t *testing.T) uuid.UUID {
	t.Helper()
	u, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() err = %v", err)
	}
	return u
}

// TestOwnerShapeIsExactlySessionAndLoop guards the spec invariant that the
// authority owner is exactly SessionID+LoopID (spec "Identity and
// authorization"): Owner must carry no other field (in particular, no
// embedded ProcessID/Handle).
func TestOwnerShapeIsExactlySessionAndLoop(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeOf(Owner{})
	if typ.NumField() != 2 {
		t.Fatalf("Owner has %d fields, want exactly 2 (SessionID, LoopID)", typ.NumField())
	}
	uuidType := reflect.TypeOf(uuid.UUID{})
	for i, name := range []string{"SessionID", "LoopID"} {
		field := typ.Field(i)
		if field.Name != name {
			t.Errorf("Owner field %d = %q, want %q", i, field.Name, name)
		}
		if field.Type != uuidType {
			t.Errorf("Owner.%s type = %v, want uuid.UUID", field.Name, field.Type)
		}
	}
}

// TestOriginShapeIsExactlyToolExecutionID guards the corresponding shape
// invariant for Origin.
func TestOriginShapeIsExactlyToolExecutionID(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeOf(Origin{})
	if typ.NumField() != 1 {
		t.Fatalf("Origin has %d fields, want exactly 1 (ToolExecutionID)", typ.NumField())
	}
	field := typ.Field(0)
	if field.Name != "ToolExecutionID" {
		t.Errorf("Origin field 0 = %q, want %q", field.Name, "ToolExecutionID")
	}
	if field.Type != reflect.TypeOf(uuid.UUID{}) {
		t.Errorf("Origin.ToolExecutionID type = %v, want uuid.UUID", field.Type)
	}
}

// TestOwnerEqual table-tests Owner.Equal across matching and mismatched
// session/loop combinations.
func TestOwnerEqual(t *testing.T) {
	t.Parallel()
	session1, session2 := mustUUID(t), mustUUID(t)
	loop1, loop2 := mustUUID(t), mustUUID(t)

	tests := []struct {
		name string
		a, b Owner
		want bool
	}{
		{
			name: "identical owner",
			a:    Owner{SessionID: session1, LoopID: loop1},
			b:    Owner{SessionID: session1, LoopID: loop1},
			want: true,
		},
		{
			name: "different session",
			a:    Owner{SessionID: session1, LoopID: loop1},
			b:    Owner{SessionID: session2, LoopID: loop1},
			want: false,
		},
		{
			name: "different loop",
			a:    Owner{SessionID: session1, LoopID: loop1},
			b:    Owner{SessionID: session1, LoopID: loop2},
			want: false,
		},
		{
			name: "different session and loop",
			a:    Owner{SessionID: session1, LoopID: loop1},
			b:    Owner{SessionID: session2, LoopID: loop2},
			want: false,
		},
		{
			name: "both zero",
			a:    Owner{},
			b:    Owner{},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.a.Equal(tt.b); got != tt.want {
				t.Errorf("Equal() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestOwnerIsZero table-tests Owner.IsZero.
func TestOwnerIsZero(t *testing.T) {
	t.Parallel()
	nonZero := mustUUID(t)
	tests := []struct {
		name string
		o    Owner
		want bool
	}{
		{name: "zero owner", o: Owner{}, want: true},
		{name: "nonzero session only", o: Owner{SessionID: nonZero}, want: false},
		{name: "nonzero loop only", o: Owner{LoopID: nonZero}, want: false},
		{name: "both nonzero", o: Owner{SessionID: nonZero, LoopID: nonZero}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.o.IsZero(); got != tt.want {
				t.Errorf("IsZero() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestOriginIsProvenanceNotAuthority proves that Origin never participates in
// ownership comparison: two identities sharing the same Owner but differing
// Origin (as every genuine follow-up call necessarily does, since each tool
// invocation mints a new ToolExecutionID) are still recognized as the same
// owner (spec "Identity and authorization": the opaque handle "is not
// compared to the execution ID of ProcessOutput, ProcessInput, or
// ProcessStop").
func TestOriginIsProvenanceNotAuthority(t *testing.T) {
	t.Parallel()
	owner := Owner{SessionID: mustUUID(t), LoopID: mustUUID(t)}
	origin1 := Origin{ToolExecutionID: mustUUID(t)}
	origin2 := Origin{ToolExecutionID: mustUUID(t)}
	if origin1 == origin2 {
		t.Fatalf("test fixture bug: expected distinct Origin values")
	}

	handle1, err := NewHandle(nil)
	if err != nil {
		t.Fatalf("NewHandle() err = %v", err)
	}
	handle2, err := NewHandle(nil)
	if err != nil {
		t.Fatalf("NewHandle() err = %v", err)
	}

	id1 := Identity{Handle: handle1, Owner: owner, Origin: origin1}
	id2 := Identity{Handle: handle2, Owner: owner, Origin: origin2}

	// A follow-up call's new ToolExecutionID (Origin) must never affect
	// whether the same Owner is recognized as authorized.
	if !id1.Owner.Equal(id2.Owner) {
		t.Fatalf("Owner.Equal() = false for identical owners with differing Origin, want true")
	}
}

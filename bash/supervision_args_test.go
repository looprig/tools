package bash

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestBashSchemaDeclaresSupervisionFields asserts the JSON schema advertises
// the new model-facing supervision arguments (background/yield_time_ms/tty/
// max_output_bytes) alongside the unchanged "timeout", and that none of them
// are required (a call using only the pre-existing fields must stay valid).
func TestBashSchemaDeclaresSupervisionFields(t *testing.T) {
	t.Parallel()
	info, err := NewBash(t.TempDir()).Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(info.Schema, &schema); err != nil {
		t.Fatalf("Schema is not the expected JSON object: %v", err)
	}
	for _, name := range []string{"timeout", "background", "yield_time_ms", "tty", "max_output_bytes"} {
		if _, ok := schema.Properties[name]; !ok {
			t.Errorf("schema is missing the %q property", name)
		}
	}
	for _, name := range schema.Required {
		if name != "command" {
			t.Errorf("schema.Required = %v, want only %q (every supervision field stays optional)", schema.Required, "command")
		}
	}
}

// TestBashSupervisionDetection pins explicit-supervision detection: a call is
// supervised exactly when it sets `background` or supplies a PRESENT (even
// zero) `yield_time_ms` — the spec's "background or yield_time_ms enables
// supervision". `tty` alone never enables it; it REQUIRES that supervision.
func TestBashSupervisionDetection(t *testing.T) {
	t.Parallel()
	zero := 0
	tests := []struct {
		name           string
		args           bashArgs
		wantSupervised bool
	}{
		{name: "no supervision fields", args: bashArgs{Command: "echo hi"}, wantSupervised: false},
		{name: "background true", args: bashArgs{Command: "echo hi", Background: true}, wantSupervised: true},
		{name: "background false explicit", args: bashArgs{Command: "echo hi", Background: false}, wantSupervised: false},
		{name: "yield_time_ms present nonzero", args: bashArgs{Command: "echo hi", YieldTimeMS: intPtr(500)}, wantSupervised: true},
		{name: "yield_time_ms present zero", args: bashArgs{Command: "echo hi", YieldTimeMS: &zero}, wantSupervised: true},
		{name: "yield_time_ms absent", args: bashArgs{Command: "echo hi"}, wantSupervised: false},
		{name: "background and yield both set", args: bashArgs{Command: "echo hi", Background: true, YieldTimeMS: intPtr(10)}, wantSupervised: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			settings, err := normalizeSupervision(tt.args)
			if err != nil {
				t.Fatalf("normalizeSupervision() error = %v", err)
			}
			if settings.Supervised != tt.wantSupervised {
				t.Errorf("Supervised = %v, want %v", settings.Supervised, tt.wantSupervised)
			}
		})
	}
}

// TestBashSupervisionValidationRanges pins range validation and the
// dependency rules between the new fields: negative timeout/yield_time_ms,
// non-positive max_output_bytes, `tty` without background/yield_time_ms, and
// an explicit `timeout: 0` without background/yield_time_ms are all rejected
// at preparation; nothing about them is deferred to execution.
func TestBashSupervisionValidationRanges(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		argsRaw string
		wantErr bool
	}{
		{name: "negative timeout", argsRaw: `{"command":"c","timeout":-1}`, wantErr: true},
		{name: "explicit zero timeout without supervision", argsRaw: `{"command":"c","timeout":0}`, wantErr: true},
		{name: "explicit zero timeout with background", argsRaw: `{"command":"c","timeout":0,"background":true}`, wantErr: false},
		{name: "explicit zero timeout with yield_time_ms", argsRaw: `{"command":"c","timeout":0,"yield_time_ms":100}`, wantErr: false},
		{name: "positive timeout with no supervision", argsRaw: `{"command":"c","timeout":5}`, wantErr: false},
		{name: "negative yield_time_ms", argsRaw: `{"command":"c","yield_time_ms":-1}`, wantErr: true},
		{name: "zero yield_time_ms", argsRaw: `{"command":"c","yield_time_ms":0}`, wantErr: false},
		{name: "tty without supervision", argsRaw: `{"command":"c","tty":true}`, wantErr: true},
		{name: "tty with background", argsRaw: `{"command":"c","tty":true,"background":true}`, wantErr: false},
		{name: "tty with yield_time_ms", argsRaw: `{"command":"c","tty":true,"yield_time_ms":50}`, wantErr: false},
		{name: "zero max_output_bytes", argsRaw: `{"command":"c","max_output_bytes":0}`, wantErr: true},
		{name: "negative max_output_bytes", argsRaw: `{"command":"c","max_output_bytes":-1}`, wantErr: true},
		{name: "positive max_output_bytes", argsRaw: `{"command":"c","max_output_bytes":1024}`, wantErr: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := NewBash(t.TempDir())
			_, _, err := b.PrepareCall(context.Background(), mustUUID(t), tt.argsRaw)
			if (err != nil) != tt.wantErr {
				t.Fatalf("PrepareCall(%s) error = %v, wantErr %v", tt.argsRaw, err, tt.wantErr)
			}
		})
	}
}

// TestBashSupervisionNoDeadlineFreezesArtifact confirms an explicit
// `timeout: 0` accepted under supervision freezes noDeadline (run until
// session shutdown) rather than falling back to the default clamp.
func TestBashSupervisionNoDeadlineFreezesArtifact(t *testing.T) {
	t.Parallel()
	b := NewBash(t.TempDir())
	_, prepared := prepareBash(t, b, `{"command":"echo hi","background":true,"timeout":0}`)
	art, ok := prepared.(*bashArtifact)
	if !ok || art == nil {
		t.Fatalf("artifact = %#v, want *bashArtifact", prepared)
	}
	if !art.noDeadline {
		t.Error("noDeadline = false, want true for a supervised timeout:0 call")
	}
}

// TestBashSupervisionDetachedSyntax pins the conservative detached-syntax
// rejection: it applies ONLY to calls that request supervision. Legacy
// foreground calls keep their existing shell compatibility unchanged,
// including a trailing '&' or a command that merely mentions nohup/setsid/
// disown as a word.
func TestBashSupervisionDetachedSyntax(t *testing.T) {
	t.Parallel()
	detachedCommands := []string{
		"sleep 999 &",
		"nohup sleep 999",
		"setsid sleep 999",
		"disown",
		"echo start && sleep 999 &",
	}
	safeCommands := []string{
		"echo hi",
		"echo a && echo b",
	}

	for _, command := range detachedCommands {
		command := command
		t.Run("legacy allows: "+command, func(t *testing.T) {
			t.Parallel()
			b := NewBash(t.TempDir())
			args, err := json.Marshal(map[string]any{"command": command})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := b.PrepareCall(context.Background(), mustUUID(t), string(args)); err != nil {
				t.Errorf("PrepareCall() error = %v, want legacy (non-supervised) calls to keep existing shell compatibility", err)
			}
		})
		t.Run("supervised rejects: "+command, func(t *testing.T) {
			t.Parallel()
			b := NewBash(t.TempDir())
			args, err := json.Marshal(map[string]any{"command": command, "background": true})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := b.PrepareCall(context.Background(), mustUUID(t), string(args)); err == nil {
				t.Errorf("PrepareCall(background:true, %q) error = nil, want a detached-syntax rejection", command)
			}
		})
	}

	for _, command := range safeCommands {
		command := command
		t.Run("supervised allows: "+command, func(t *testing.T) {
			t.Parallel()
			b := NewBash(t.TempDir())
			args, err := json.Marshal(map[string]any{"command": command, "background": true})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := b.PrepareCall(context.Background(), mustUUID(t), string(args)); err != nil {
				t.Errorf("PrepareCall(background:true, %q) error = %v, want no detached-syntax false positive", command, err)
			}
		})
	}
}

// TestBashLegacyPreservesSyncPathWithFalseSupervisionFields proves a call that
// carries the new fields but at their legacy-equivalent zero values
// (background:false, tty:false, no yield_time_ms/max_output_bytes) still
// follows the existing synchronous path: identical plain text and exit
// marker to the same call made with only the pre-existing fields.
func TestBashLegacyPreservesSyncPathWithFalseSupervisionFields(t *testing.T) {
	t.Parallel()
	requireSh(t)
	root := t.TempDir()

	legacy := runBash(t, root, map[string]any{"command": "echo hello"})
	withFalseFields := runBash(t, root, map[string]any{
		"command":    "echo hello",
		"background": false,
		"tty":        false,
	})
	if legacy != withFalseFields {
		t.Errorf("result with explicit-false supervision fields = %q, want identical to legacy %q", withFalseFields, legacy)
	}
	if !strings.Contains(withFalseFields, "[exit code: 0]") {
		t.Errorf("result %q missing the exit marker", withFalseFields)
	}
}

// intPtr returns a pointer to v, for building presence-aware bashArgs in
// table-driven tests.
func intPtr(v int) *int { return &v }

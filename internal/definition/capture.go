package definition

import "github.com/looprig/harness/pkg/tool"

// captureSafetyDefinition is a tool.Definition that also declares its capture
// safety (tool.CaptureSafetyDeclarer). Harness's own constructors return a
// sealed Definition with no such method, so the declaration is carried by
// embedding: every Definition method, including the unexported seal, is
// promoted unchanged.
type captureSafetyDefinition struct {
	tool.Definition
	declared tool.DeclaredCaptureSafety
}

// DeclaredCaptureSafety implements tool.CaptureSafetyDeclarer.
func (d *captureSafetyDefinition) DeclaredCaptureSafety() tool.DeclaredCaptureSafety {
	return d.declared
}

// WithCaptureSafety returns inner declaring declared. Every standard
// definition this module exports is wrapped, so an assembly's capture-safety
// descriptor reflects what each tool actually does rather than Harness's
// declare-nothing default.
func WithCaptureSafety(inner tool.Definition, declared tool.DeclaredCaptureSafety) tool.Definition {
	return &captureSafetyDefinition{Definition: inner, declared: declared}
}

var _ tool.CaptureSafetyDeclarer = (*captureSafetyDefinition)(nil)

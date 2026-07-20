package filemutation

// prepare.go holds the preparation seam shared by WriteFile and EditFile (the
// two direct mutation tools). Preparation is the SINGLE argument-parsing and
// canonicalization step: PrepareCall decodes the untrusted argsJSON once,
// proves workspace containment once, and binds the validated result into a
// typed artifact the tool executes WITHOUT ever reparsing the raw JSON. The
// same preparation resolves the canonical write scheduling key, so the
// runner's WriteTarget grouping and the requirement Scope can never diverge.
//
// The emitted requirement is a DIRECT filesystem.write: Scope and Match are
// both the canonical resolved target path and the grant pair is EMPTY — the
// tool enforces the approved resolved path itself at run time by re-resolving
// the path and refusing to proceed when the resolution no longer equals the
// approved canonical target (a parent-directory symlink swap between prepare
// and run redirects the write nowhere).

import (
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/internal/workspace"
	"github.com/looprig/tools/permission"
)

// mutationTarget is the validated, canonicalized target every prepared
// mutation artifact carries. abs is the canonical symlink-resolved path (the
// requirement Scope/Match, the scheduling key, and the observation key);
// lexical is the on-disk name the atomic write targets (never following a
// final-component symlink); display is the model-supplied path used only in
// messages.
type mutationTarget struct {
	abs     string
	lexical string
	display string
}

// resolveMutationTarget is the shared parse-free containment step: it proves
// the symlink-resolved target is inside the workspace and returns the three
// path forms of the target. display must be non-empty (callers validate).
func resolveMutationTarget(root, display string) (mutationTarget, error) {
	abs, err := workspace.ContainedPath(root, display)
	if err != nil {
		return mutationTarget{}, &writeFileError{reason: "path is outside the workspace", cause: err}
	}
	return mutationTarget{abs: abs, lexical: workspace.JoinedPath(root, display), display: display}, nil
}

// writeRequirement builds the single direct filesystem.write requirement for
// one canonical resolved target path, with the matching reusable exact-path
// allow candidate (empty grant pair on both: the tool enforces the approved
// path itself).
func writeRequirement(abs string) tool.Requirement {
	return tool.Requirement{
		Kind:        permission.CapabilityFilesystemWrite,
		Scope:       abs,
		Match:       abs,
		Description: "write " + abs,
		Candidates: []tool.RuleCandidate{{
			Kind:        permission.CapabilityFilesystemWrite,
			Match:       abs,
			Description: "write " + abs,
		}},
	}
}

// mutationRequest assembles the prepared request for one direct mutation call.
func mutationRequest(toolName, executionID string, target mutationTarget) tool.Request {
	return tool.Request{
		ToolName:     toolName,
		ExecutionID:  executionID,
		Requirements: []tool.Requirement{writeRequirement(target.abs)},
	}
}

// enforceApprovedResolution re-proves at RUN time that the display path still
// resolves to the APPROVED canonical target. Preparation resolved once for the
// permission decision; a parent-directory swap between prepare and run would
// silently redirect the lexical write, so a changed resolution refuses the
// mutation (fail closed) rather than writing anywhere else.
func enforceApprovedResolution(root string, target mutationTarget) error {
	abs, err := workspace.ContainedPath(root, target.display)
	if err != nil {
		return &writeFileError{reason: "path is outside the workspace", cause: err}
	}
	if abs != target.abs {
		return &writeFileError{reason: "path resolution changed since approval"}
	}
	return nil
}

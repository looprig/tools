# Tool Package Layout Implementation Plan

**Goal:** Reorganize the uncommitted tools extraction into independent tool packages with a thin root definition facade.

**Architecture:** Each concrete tool owns one public package. Cross-tool path and mutation mechanics live in private internal packages. The root package composes concrete tools into harness definitions, while advanced consumers import only the focused packages whose options they need.

**Tech Stack:** Go 1.26, harness tool contracts, standard Go dependency tests, race detector, Staticcheck, Gosec.

---

### Task 1: Lock the package boundary with a failing test

**Files:**
- Modify: `dependency_test.go`

1. Add assertions that production tool implementations do not remain at module root.
2. Add assertions that required focused packages exist.
3. Run the focused boundary test and confirm it fails against the flat layout.

### Task 2: Extract shared workspace mechanics

**Files:**
- Move: containment and glob-matching implementation and tests to `internal/workspace`
- Move: structured mutation permit implementation and tests to `internal/filemutation`

1. Export only the functions and types needed by sibling tool packages.
2. Keep detailed helpers private inside each internal package.
3. Run internal package tests.

### Task 3: Move every concrete tool into its own package

**Files:**
- Move tool implementation and focused tests into `askuser`, `bash`, `editfile`, `fetch`, `glob`, `grep`, `readfile`, `skill`, `todo`, `websearch`, and `writefile`
- Move permission implementation and tests into `permission`

1. Move production files with their owning tests.
2. Update package declarations and imports.
3. Replace shared root helper calls with the new internal package APIs.
4. Compile packages incrementally and resolve only boundary-related failures.

### Task 4: Reduce the root to a definition facade

**Files:**
- Modify: `definitions.go`
- Modify: `definitions_test.go`
- Modify: `README.md`
- Modify: `docs/specs/module.md`

1. Build each definition using its focused package.
2. Keep existing root definition-builder names.
3. Update examples and package documentation to describe simple and advanced imports.
4. Run root facade tests and the dependency boundary test.

### Task 5: Migrate first-party consumers

**Files:**
- Modify: `../confinement/*.go`
- Modify: `../coderig/*.go`

1. Move permission references to `tools/permission`.
2. Move Bash and Grep option references to their focused packages.
3. Move Skill and WebSearch provider references to their focused packages.
4. Preserve behavior and public APIs of coderig and confinement.

### Task 6: Verify and commit

1. Run formatting checks across tools, coderig, and confinement.
2. Run vet, Staticcheck, Gosec, module verification, race tests, and builds for all three repositories.
3. Confirm unrelated repositories remain untouched.
4. Commit the tools refactor and required first-party import migrations as separate repository commits.

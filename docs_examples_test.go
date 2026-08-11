package tools_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const offlineExamplesCommand = "GOWORK=off GOCACHE=/tmp/looprig-tools-docs-gocache go test ./examples/..."

type examplesManifest struct {
	SchemaVersion int    `json:"schemaVersion"`
	Repository    string `json:"repository"`
	ProofSources  []struct {
		ID     string `json:"id"`
		Type   string `json:"type"`
		Path   string `json:"path"`
		Symbol string `json:"symbol,omitempty"`
	} `json:"proofSources"`
	Examples []struct {
		ID             string            `json:"id"`
		Ecosystem      string            `json:"ecosystem"`
		Owner          string            `json:"owner"`
		SourcePath     string            `json:"sourcePath"`
		Availability   string            `json:"availability"`
		Versions       map[string]string `json:"versions"`
		OfflineCommand string            `json:"offlineCommand"`
		Assertion      string            `json:"assertion"`
		WorkflowPath   string            `json:"workflowPath"`
		JobID          string            `json:"jobId"`
		Cleanup        string            `json:"cleanup"`
		LiveGate       any               `json:"liveGate"`
		ProofIDs       []string          `json:"proofIds"`
	} `json:"examples"`
}

func TestDocsExamplesArtifacts(t *testing.T) {
	t.Parallel()

	wantFixtures := map[string]string{
		"examples/definitions/example_test.go": "Example_definitionRequirements",
		"examples/preparation/example_test.go": "Example_preparedBashCall",
		"examples/processes/example_test.go":   "Example_processLifecycle",
		"examples/skills/example_test.go":      "Example_embeddedSkill",
		"examples/tasks/example_test.go":       "Example_taskBundle",
		"examples/permissions/example_test.go": "Example_exactPermissionRules",
	}
	for path := range wantFixtures {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("runnable example %q: %v", path, err)
		}
	}

	raw, err := os.ReadFile(filepath.Join("testdata", "docs", "examples.json"))
	if err != nil {
		t.Fatalf("read examples manifest: %v", err)
	}
	var manifest examplesManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("decode examples manifest: %v", err)
	}
	if manifest.SchemaVersion != 1 || manifest.Repository != "tools" {
		t.Fatalf("manifest identity = schema %d repository %q", manifest.SchemaVersion, manifest.Repository)
	}

	wantProofs := make(map[string]struct {
		typeName string
		path     string
		symbol   string
	}, len(wantFixtures)+1)
	for path, symbol := range wantFixtures {
		name := strings.TrimSuffix(strings.TrimPrefix(path, "examples/"), "/example_test.go")
		wantProofs["example-tools-"+name+"-fixture"] = struct {
			typeName string
			path     string
			symbol   string
		}{"executable-fixture", path, symbol}
	}
	wantProofs["example-tools-manifest-contract-test"] = struct {
		typeName string
		path     string
		symbol   string
	}{"test", "docs_examples_test.go", "TestDocsExamplesArtifacts"}

	proofs := make(map[string]bool, len(manifest.ProofSources))
	for _, proof := range manifest.ProofSources {
		want, ok := wantProofs[proof.ID]
		if !ok {
			t.Errorf("unexpected proof source ID %q", proof.ID)
			continue
		}
		if proof.Type != want.typeName || proof.Path != want.path || proof.Symbol != want.symbol {
			t.Errorf("proof %q = type %q path %q symbol %q, want type %q path %q symbol %q", proof.ID, proof.Type, proof.Path, proof.Symbol, want.typeName, want.path, want.symbol)
		}
		if strings.Contains(proof.Path, "#") {
			t.Errorf("proof %q path contains symbol fragment: %q", proof.ID, proof.Path)
		}
		if _, err := os.Stat(proof.Path); err != nil {
			t.Errorf("proof %q path does not resolve: %v", proof.ID, err)
		}
		proofs[proof.ID] = true
	}
	if len(manifest.ProofSources) != len(wantProofs) {
		t.Errorf("proof source count = %d, want %d", len(manifest.ProofSources), len(wantProofs))
	}
	if len(manifest.Examples) != len(wantFixtures) {
		t.Fatalf("manifest examples = %d, want %d", len(manifest.Examples), len(wantFixtures))
	}
	seen := make(map[string]bool, len(manifest.Examples))
	for _, example := range manifest.Examples {
		if seen[example.ID] {
			t.Errorf("duplicate example ID %q", example.ID)
		}
		seen[example.ID] = true
		if !strings.HasPrefix(example.ID, "example-tools-") || example.Ecosystem != "go" || example.Owner != "tools" || example.Availability != "source-workspace" {
			t.Errorf("example %q classification is incorrect", example.ID)
		}
		if example.Versions["github.com/looprig/tools"] != "source-workspace" || len(example.Versions) != 1 {
			t.Errorf("example %q versions = %#v", example.ID, example.Versions)
		}
		if example.OfflineCommand != offlineExamplesCommand {
			t.Errorf("example %q offlineCommand = %q", example.ID, example.OfflineCommand)
		}
		if _, ok := wantFixtures[example.SourcePath]; !ok {
			t.Errorf("example %q sourcePath = %q", example.ID, example.SourcePath)
		}
		if example.Assertion == "" || example.WorkflowPath != ".github/workflows/docs-examples.yml" || example.JobID != "docs-examples" || example.Cleanup == "" || example.LiveGate != nil {
			t.Errorf("example %q has incomplete execution metadata", example.ID)
		}
		if len(example.ProofIDs) < 2 {
			t.Errorf("example %q proofIds = %v, want source and test proofs", example.ID, example.ProofIDs)
		}
		for _, proofID := range example.ProofIDs {
			if !proofs[proofID] {
				t.Errorf("example %q references unknown proof %q", example.ID, proofID)
			}
		}
	}

	workflow, err := os.ReadFile(filepath.Join(".github", "workflows", "docs-examples.yml"))
	if err != nil {
		t.Fatalf("read docs examples workflow: %v", err)
	}
	for _, literal := range []string{
		"docs-examples:",
		offlineExamplesCommand,
		"GOWORK=off GOCACHE=/tmp/looprig-tools-docs-gocache make test",
		"GOWORK=off GOCACHE=/tmp/looprig-tools-docs-gocache go test -race ./...",
	} {
		if !strings.Contains(string(workflow), literal) {
			t.Errorf("workflow does not contain %q", literal)
		}
	}
}

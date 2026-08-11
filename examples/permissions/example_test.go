package permissions_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/permission"
)

// Example_exactPermissionRules persists the exact candidate shown to a user.
// A rule for one command does not silently authorize a neighboring command.
func Example_exactPermissionRules() {
	dir, err := os.MkdirTemp("", "looprig-tools-permissions-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	store, _, err := permission.NewWorkspaceStore(permission.Config{
		Path: filepath.Join(dir, "permissions.json"),
	})
	if err != nil {
		panic(err)
	}
	const approved = "go test ./..."
	err = store.WriteRules(context.Background(), []tool.RuleCandidate{{
		Kind:        tool.CapabilityCommandExecute,
		Match:       approved,
		GrantClass:  tool.GrantClassCommandStart,
		GrantTarget: approved,
		Description: "run the repository tests",
	}})
	if err != nil {
		panic(err)
	}

	exact, err := store.MatchesAllow(context.Background(), tool.Requirement{Kind: tool.CapabilityCommandExecute, Match: approved})
	if err != nil {
		panic(err)
	}
	unrelated, err := store.MatchesAllow(context.Background(), tool.Requirement{Kind: tool.CapabilityCommandExecute, Match: "go test ./private/..."})
	if err != nil {
		panic(err)
	}
	fmt.Println("approved:", exact)
	fmt.Println("unrelated:", unrelated)

	// Output:
	// approved: true
	// unrelated: false
}

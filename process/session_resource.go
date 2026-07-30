package process

import (
	"context"

	"github.com/looprig/harness/pkg/tool"
)

// SupervisorResourceKey names the ONE shared process.Supervisor session
// resource every supervised Bash call (bash/supervised.go's runSupervised)
// and every ProcessOutput/ProcessInput/ProcessStop call (this module's root
// definitions.go) obtain through tool.SessionResourceRegistry.GetOrCreate:
// "any of the four definitions may win get-or-create" requires all of them
// to key on this exact same string. Both consumer packages import this one
// constant rather than each keeping their own private copy, so there is a
// single authoritative source for the key.
const SupervisorResourceKey = "github.com/looprig/tools/process.supervisor"

// SupervisorResource adapts the shared *Supervisor (and its backing
// *ManifestStore) to tool.SessionResource, so it can be obtained through
// tool.SessionResourceRegistry.GetOrCreate by any of the four process-backed
// tool definitions (Bash's supervised path, ProcessOutput, ProcessInput,
// ProcessStop). GetOrCreate's factory runs exactly once per key: whichever
// caller's GetOrCreate reaches SupervisorResourceKey first determines the
// concrete tool.SessionResource value every later caller with that key
// receives back, including a caller in a different package. Exporting one
// shared type/factory here (rather than each consumer package keeping its
// own private wrapper) is what makes that safe: every caller, regardless of
// which one wins the race, type-asserts the resource to the same
// *SupervisorResource.
type SupervisorResource struct {
	Supervisor *Supervisor
	Manifests  *ManifestStore
}

// Activate is intentionally a no-op today: the Supervisor this resource
// wraps is constructed notification-free (NewSupervisorResource passes nil
// for both NewSupervisor's lifecycle and notifications parameters). A future
// task that wires services's real capabilities through to a live Supervisor
// is what would give Activate real work to do.
func (r *SupervisorResource) Activate(context.Context, tool.SessionResourceServices) error {
	return nil
}

// Shutdown releases every resource the shared Supervisor still holds.
func (r *SupervisorResource) Shutdown(ctx context.Context) error {
	return r.Supervisor.Shutdown(ctx)
}

// NewSupervisorResource is the tool.SessionResourceRegistry.GetOrCreate
// factory for SupervisorResourceKey: it is runner-free (constructs no
// tool.AsyncProcessRunner and calls neither PrepareProcess nor Start), so any
// of the four process-backed definitions may win the get-or-create race for
// a session's shared supervisor. dir is the private per-session storage
// directory the registry reserves for this key.
func NewSupervisorResource(dir string) (tool.SessionResource, error) {
	manifests := NewManifestStore(dir)
	supervisor, err := NewSupervisor(Config{}, manifests, dir, nil, nil)
	if err != nil {
		return nil, err
	}
	return &SupervisorResource{Supervisor: supervisor, Manifests: manifests}, nil
}

var _ tool.SessionResource = (*SupervisorResource)(nil)

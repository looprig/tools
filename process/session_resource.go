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

// Activate wires the real, validated tool.SessionResourceServices Harness's
// live session construction supplies into the shared Supervisor this
// resource wraps. NewSupervisorResource itself constructs the Supervisor
// notification-free (nil lifecycle/notifications at NewSupervisor time,
// below) precisely because these real capabilities do not exist yet at
// factory time -- SessionResource's own contract late-binds them here, after
// the live session, hub, durable publisher, and notifier are ready
// (pkg/tool.SessionResource's doc comment). Activate adapts services'
// validated tool.ProcessLifecyclePublisher/tool.ProcessCompletionNotifier
// into this package's private lifecycleSink/completionNotifier shapes
// (lifecycle_bridge.go) and installs them on the live Supervisor
// (activateServices), which every subsequent Start call -- and so every
// admitted process's Start-time and terminal lifecycle publish and
// completion notify -- reads from that point on (supervisor.go's
// servicesLocked). services.Validate() rejects a nil or typed-nil service
// before either is installed, so a caller that mishandles construction can
// never leave the Supervisor half-wired.
func (r *SupervisorResource) Activate(_ context.Context, services tool.SessionResourceServices) error {
	if err := services.Validate(); err != nil {
		return err
	}
	r.Supervisor.activateServices(
		lifecyclePublisherAdapter{publisher: services.ProcessLifecyclePublisher()},
		completionNotifierAdapter{notifier: services.ProcessCompletionNotifier()},
	)
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

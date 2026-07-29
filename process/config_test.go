package process

import (
	"errors"
	"testing"
	"time"
)

// TestDefaultsAreSelfConsistent regression-guards the relationship the task
// requires: a per-process default must never exceed its aggregate/broader
// counterpart default. If someone edits one constant without the other, this
// test catches it.
func TestDefaultsAreSelfConsistent(t *testing.T) {
	t.Parallel()
	if DefaultMaxProcessInMemoryBytes > DefaultMaxAggregateInMemoryBytes {
		t.Errorf("DefaultMaxProcessInMemoryBytes (%d) > DefaultMaxAggregateInMemoryBytes (%d)", DefaultMaxProcessInMemoryBytes, DefaultMaxAggregateInMemoryBytes)
	}
	if DefaultMaxProcessSpoolBytes > DefaultMaxAggregateSpoolBytes {
		t.Errorf("DefaultMaxProcessSpoolBytes (%d) > DefaultMaxAggregateSpoolBytes (%d)", DefaultMaxProcessSpoolBytes, DefaultMaxAggregateSpoolBytes)
	}
	if DefaultMaxRunningProcessesPerLoop > DefaultMaxRunningProcessesPerSession {
		t.Errorf("DefaultMaxRunningProcessesPerLoop (%d) > DefaultMaxRunningProcessesPerSession (%d)", DefaultMaxRunningProcessesPerLoop, DefaultMaxRunningProcessesPerSession)
	}
}

// TestConfigNormalizeZeroValueGetsDocumentedDefaults verifies the zero
// Config normalizes, with no error, to exactly the documented defaults.
func TestConfigNormalizeZeroValueGetsDocumentedDefaults(t *testing.T) {
	t.Parallel()
	got, err := Config{}.Normalize()
	if err != nil {
		t.Fatalf("Normalize() err = %v, want nil", err)
	}
	want := Config{
		MaxRunningProcessesPerLoop:              DefaultMaxRunningProcessesPerLoop,
		MaxRunningProcessesPerSession:           DefaultMaxRunningProcessesPerSession,
		MaxRetainedCompletedProcessesPerSession: DefaultMaxRetainedCompletedProcessesPerSession,
		MaxProcessInMemoryBytes:                 DefaultMaxProcessInMemoryBytes,
		MaxAggregateInMemoryBytes:               DefaultMaxAggregateInMemoryBytes,
		MaxProcessSpoolBytes:                    DefaultMaxProcessSpoolBytes,
		MaxAggregateSpoolBytes:                  DefaultMaxAggregateSpoolBytes,
		MaxInlineResultBytes:                    DefaultMaxInlineResultBytes,
		MaxPendingWaiters:                       DefaultMaxPendingWaiters,
		MaxPendingInputBytes:                    DefaultMaxPendingInputBytes,
		GracefulShutdownPeriod:                  DefaultGracefulShutdownPeriod,
	}
	if got != want {
		t.Errorf("Normalize() = %+v, want %+v", got, want)
	}
}

// TestConfigNormalizePartialOverrideKeepsExplicitFields verifies Normalize
// only fills zero fields, leaving an explicit nonzero value untouched.
func TestConfigNormalizePartialOverrideKeepsExplicitFields(t *testing.T) {
	t.Parallel()
	in := Config{MaxRunningProcessesPerSession: 5, MaxProcessSpoolBytes: 10}
	got, err := in.Normalize()
	if err != nil {
		t.Fatalf("Normalize() err = %v, want nil", err)
	}
	if got.MaxRunningProcessesPerSession != 5 {
		t.Errorf("MaxRunningProcessesPerSession = %d, want explicit 5", got.MaxRunningProcessesPerSession)
	}
	if got.MaxProcessSpoolBytes != 10 {
		t.Errorf("MaxProcessSpoolBytes = %d, want explicit 10", got.MaxProcessSpoolBytes)
	}
	if got.MaxRunningProcessesPerLoop != DefaultMaxRunningProcessesPerLoop {
		t.Errorf("MaxRunningProcessesPerLoop = %d, want default %d", got.MaxRunningProcessesPerLoop, DefaultMaxRunningProcessesPerLoop)
	}
	if got.GracefulShutdownPeriod != DefaultGracefulShutdownPeriod {
		t.Errorf("GracefulShutdownPeriod = %v, want default %v", got.GracefulShutdownPeriod, DefaultGracefulShutdownPeriod)
	}
}

// TestConfigValidateRejectsNegativeLimits table-tests that every field
// rejects a negative value with CodeInvalidSettings, while the zero value for
// that same field is accepted (zero means "use the default", not "invalid").
func TestConfigValidateRejectsNegativeLimits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "negative max running per loop", cfg: Config{MaxRunningProcessesPerLoop: -1}},
		{name: "negative max running per session", cfg: Config{MaxRunningProcessesPerSession: -1}},
		{name: "negative max retained completed", cfg: Config{MaxRetainedCompletedProcessesPerSession: -1}},
		{name: "negative max process in-memory bytes", cfg: Config{MaxProcessInMemoryBytes: -1}},
		{name: "negative max aggregate in-memory bytes", cfg: Config{MaxAggregateInMemoryBytes: -1}},
		{name: "negative max process spool bytes", cfg: Config{MaxProcessSpoolBytes: -1}},
		{name: "negative max aggregate spool bytes", cfg: Config{MaxAggregateSpoolBytes: -1}},
		{name: "negative max inline result bytes", cfg: Config{MaxInlineResultBytes: -1}},
		{name: "negative max pending waiters", cfg: Config{MaxPendingWaiters: -1}},
		{name: "negative max pending input bytes", cfg: Config{MaxPendingInputBytes: -1}},
		{name: "negative graceful shutdown period", cfg: Config{GracefulShutdownPeriod: -1 * time.Second}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() err = nil, want CodeInvalidSettings error")
			}
			if !errors.Is(err, New(CodeInvalidSettings)) {
				t.Errorf("Validate() err = %v, want CodeInvalidSettings", err)
			}
			if _, normErr := tt.cfg.Normalize(); normErr == nil {
				t.Errorf("Normalize() err = nil, want error for invalid config")
			}
		})
	}
}

func TestConfigValidateZeroIsAccepted(t *testing.T) {
	t.Parallel()
	if err := (Config{}).Validate(); err != nil {
		t.Errorf("Validate() on zero Config err = %v, want nil (zero means unset/default)", err)
	}
}

// TestConfigValidatePerProcessCannotExceedAggregate covers the cross-field
// invariant the task specifies explicitly: a per-process value cannot exceed
// its aggregate limit. Both explicit nonzero values are required to trigger
// the check; either being zero (unset) does not.
func TestConfigValidatePerProcessCannotExceedAggregate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			name:    "process in-memory exceeds aggregate in-memory",
			cfg:     Config{MaxProcessInMemoryBytes: 100, MaxAggregateInMemoryBytes: 50},
			wantErr: true,
		},
		{
			name:    "process in-memory equal to aggregate in-memory is fine",
			cfg:     Config{MaxProcessInMemoryBytes: 50, MaxAggregateInMemoryBytes: 50},
			wantErr: false,
		},
		{
			name:    "process spool exceeds aggregate spool",
			cfg:     Config{MaxProcessSpoolBytes: 100, MaxAggregateSpoolBytes: 50},
			wantErr: true,
		},
		{
			name:    "process spool under aggregate spool is fine",
			cfg:     Config{MaxProcessSpoolBytes: 10, MaxAggregateSpoolBytes: 50},
			wantErr: false,
		},
		{
			name:    "per-loop exceeds per-session",
			cfg:     Config{MaxRunningProcessesPerLoop: 10, MaxRunningProcessesPerSession: 5},
			wantErr: true,
		},
		{
			name:    "per-loop under per-session is fine",
			cfg:     Config{MaxRunningProcessesPerLoop: 2, MaxRunningProcessesPerSession: 5},
			wantErr: false,
		},
		{
			name:    "per-process set, aggregate unset (zero) does not trigger the cross-check",
			cfg:     Config{MaxProcessSpoolBytes: 1_000_000_000},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(err, New(CodeInvalidSettings)) {
				t.Errorf("Validate() err = %v, want CodeInvalidSettings", err)
			}
		})
	}
}

// TestConfigNormalizeInvalidReturnsZeroConfig verifies a failed Normalize
// returns the zero Config alongside the error, never a partially-defaulted
// value.
func TestConfigNormalizeInvalidReturnsZeroConfig(t *testing.T) {
	t.Parallel()
	got, err := Config{MaxRunningProcessesPerLoop: -1}.Normalize()
	if err == nil {
		t.Fatalf("Normalize() err = nil, want error")
	}
	if got != (Config{}) {
		t.Errorf("Normalize() = %+v on error, want zero Config", got)
	}
}

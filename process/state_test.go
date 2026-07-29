package process

import (
	"errors"
	"testing"
)

// allDefinedStates lists every externally visible state, independent of any
// production slice/map, so tests exercise the full domain without relying on
// the implementation's own enumeration.
var allDefinedStates = []State{
	StateStarting,
	StateRunning,
	StateExited,
	StateFailed,
	StateTimedOut,
	StateInterrupted,
	StateTerminated,
	StateKilled,
	StateLostOnRestore,
}

// terminalDefinedStates lists exactly the states the spec documents as
// terminal (spec "State machine").
var terminalDefinedStates = map[State]bool{
	StateExited:        true,
	StateFailed:        true,
	StateTimedOut:      true,
	StateInterrupted:   true,
	StateTerminated:    true,
	StateKilled:        true,
	StateLostOnRestore: true,
}

func TestStateValid(t *testing.T) {
	t.Parallel()
	for _, s := range allDefinedStates {
		if !s.Valid() {
			t.Errorf("State(%q).Valid() = false, want true", s)
		}
	}
	for _, bad := range []State{"", "bogus", "STARTING", "running "} {
		if bad.Valid() {
			t.Errorf("State(%q).Valid() = true, want false", bad)
		}
	}
}

func TestStateTerminal(t *testing.T) {
	t.Parallel()
	for _, s := range allDefinedStates {
		want := terminalDefinedStates[s]
		if got := s.Terminal(); got != want {
			t.Errorf("State(%q).Terminal() = %v, want %v", s, got, want)
		}
	}
}

// wantEdges is an independently authored table of every approved state
// transition edge from the spec's "State machine" section, used to check
// transition/canTransition without re-deriving the answer from the
// production `transitions` map.
var wantEdges = map[State]map[State]bool{
	StateStarting: {
		StateRunning:       true,
		StateFailed:        true,
		StateTerminated:    true,
		StateKilled:        true,
		StateLostOnRestore: true,
	},
	StateRunning: {
		StateExited:        true,
		StateFailed:        true,
		StateTimedOut:      true,
		StateTerminated:    true,
		StateKilled:        true,
		StateInterrupted:   true,
		StateLostOnRestore: true,
	},
}

// TestStateTransitionMatrix exhaustively checks every (from, to) pair over
// the full defined-state domain plus a handful of unrecognized states,
// proving: only approved transitions are accepted, and terminal states never
// transition (including a self-transition and a transition to an
// unrecognized state).
func TestStateTransitionMatrix(t *testing.T) {
	t.Parallel()
	universe := append(append([]State{}, allDefinedStates...), "", "bogus")

	for _, from := range universe {
		for _, to := range universe {
			wantOK := wantEdges[from][to]

			gotOK := canTransition(from, to)
			if gotOK != wantOK {
				t.Errorf("canTransition(%q, %q) = %v, want %v", from, to, gotOK, wantOK)
			}

			got, err := transition(from, to)
			if wantOK {
				if err != nil {
					t.Errorf("transition(%q, %q) err = %v, want nil", from, to, err)
				}
				if got != to {
					t.Errorf("transition(%q, %q) = %q, want %q", from, to, got, to)
				}
				continue
			}
			if err == nil {
				t.Errorf("transition(%q, %q) err = nil, want *TransitionError", from, to)
				continue
			}
			if got != from {
				t.Errorf("transition(%q, %q) = %q on rejected edge, want unchanged %q", from, to, got, from)
			}
			var transErr *TransitionError
			if !errors.As(err, &transErr) {
				t.Errorf("transition(%q, %q) err = %v, want *TransitionError", from, to, err)
				continue
			}
			if transErr.From != from || transErr.To != to {
				t.Errorf("TransitionError = {From: %q, To: %q}, want {From: %q, To: %q}", transErr.From, transErr.To, from, to)
			}
		}
	}
}

// TestTerminalStatesNeverTransition is a focused restatement of the terminal
// slice of TestStateTransitionMatrix: every terminal state rejects every
// possible destination, including itself.
func TestTerminalStatesNeverTransition(t *testing.T) {
	t.Parallel()
	destinations := append(append([]State{}, allDefinedStates...), "", "bogus")
	for terminalState := range terminalDefinedStates {
		for _, to := range destinations {
			if canTransition(terminalState, to) {
				t.Errorf("canTransition(%q, %q) = true, want false: terminal states must never transition", terminalState, to)
			}
		}
	}
}

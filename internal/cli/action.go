package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Input is what an action receives. ActionID, when set, is adopted instead of generating a new id
// (propagation from the CLI through the control API into the supervisor).
type Input struct {
	ActionID string
	Target   string         // what the action acts on (logged as target), e.g. a camera id
	Args     map[string]any // handler-defined parameters
}

// Output is what an action returns. Target, when set, replaces Input.Target in the result record.
type Output struct {
	Target string
	Data   any
}

// Action is one registered operation. Name is "noun.verb" (for example "camera.add").
type Action struct {
	Name      string
	Component string // logger component: cli, host, supervisor, relay, web, auth, ctlapi, ...
	Mutating  bool   // changes state: a successful result is logged at INFO instead of DEBUG; implies Audit
	Audit     bool   // security-relevant: its records also go to the audit stream (Register sets it for every Mutating action)
	Run       func(ctx context.Context, in Input) (Output, error)
}

// ErrRefused marks a deliberate refusal (a guard, a lockout, a validation); wrap it with Refuse.
var ErrRefused = errors.New("refused")

// Refuse returns an error that Is ErrRefused and carries the reason.
func Refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, args...))
}

// Registry holds the actions by name.
type Registry struct {
	mu      sync.RWMutex
	actions map[string]Action
}

func NewRegistry() *Registry { return &Registry{actions: map[string]Action{}} }

// Default is the registry packages register into from init functions.
var Default = NewRegistry()

// Register adds the action to the Default registry; it panics on a duplicate or invalid action.
func Register(a Action) { Default.Register(a) }

// Register adds an action. A duplicate name, an empty name or component, or a nil Run panics, so
// the mistake surfaces at program start. Every Mutating action is registered as Audit.
func (r *Registry) Register(a Action) {
	if a.Name == "" || a.Component == "" || a.Run == nil {
		panic(fmt.Sprintf("cli: invalid action %+v: Name, Component and Run are required", a.Name))
	}
	if a.Mutating {
		a.Audit = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.actions[a.Name]; dup {
		panic("cli: duplicate action " + a.Name)
	}
	r.actions[a.Name] = a
}

// All returns every action sorted by name.
func (r *Registry) All() []Action {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Action, 0, len(r.actions))
	for _, a := range r.actions {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Lookup finds an action by name.
func (r *Registry) Lookup(name string) (Action, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.actions[name]
	return a, ok
}

// Exposed is a list of command, route or handler names that a package makes reachable. Each must
// be a registered Action name; see AssertAllExposedAreActions.
type Exposed []string

// TB is the part of testing.TB the assertion needs (kept here so production code does not import testing).
type TB interface {
	Helper()
	Errorf(format string, args ...any)
}

// AssertAllExposedAreActions fails the test when any exposed name is not in the Default registry,
// so a new command, control-API handler or web handler cannot be added without being an Action.
func AssertAllExposedAreActions(t TB, exposed Exposed) {
	t.Helper()
	AssertExposedIn(t, Default, exposed)
}

// AssertExposedIn is AssertAllExposedAreActions against a specific registry.
func AssertExposedIn(t TB, r *Registry, exposed Exposed) {
	t.Helper()
	for _, name := range exposed {
		if _, ok := r.Lookup(name); !ok {
			t.Errorf("%q is exposed but is not a registered cli.Action: register it so its start and result are logged", name)
		}
	}
}

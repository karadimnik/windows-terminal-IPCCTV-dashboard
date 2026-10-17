package cli

import (
	"context"
	"os"
	"os/user"
)

// Actors that are not people.
const (
	ActorHost       = "host"
	ActorSupervisor = "supervisor"
	ActorSystem     = "system"
)

type ctxKey int

const (
	actorKey ctxKey = iota
	actionIDKey
)

// CLIActor is "cli:<os user>".
func CLIActor() string {
	name := ""
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	if name == "" {
		name = os.Getenv("USER")
	}
	if name == "" {
		name = os.Getenv("USERNAME")
	}
	if name == "" {
		name = "unknown"
	}
	return "cli:" + name
}

// WebActor is "web:<name>".
func WebActor(name string) string { return "web:" + name }

// WithActor records who performs the action.
func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey, actor)
}

// ActorFrom returns the actor in ctx, or "system".
func ActorFrom(ctx context.Context) string {
	if a, _ := ctx.Value(actorKey).(string); a != "" {
		return a
	}
	return ActorSystem
}

// WithActionID carries an action id across calls and processes.
func WithActionID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, actionIDKey, id)
}

// ActionIDFrom returns the action id in ctx, or "".
func ActionIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(actionIDKey).(string)
	return id
}

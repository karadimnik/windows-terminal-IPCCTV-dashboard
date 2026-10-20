package host

import (
	"context"
	"io"

	"holocron/internal/osx"
)

// ExecSpawner starts the supervisor as a child inside the host's own osx.Scope, so the supervisor
// dies with the host however abruptly (and its own scope then takes its children). On Windows the
// child gets its own hidden console (CREATE_NO_WINDOW): a window close reaches only the host, which
// runs the ordered stop. The command tree (W9) supplies the current executable and its hidden
// "supervisor" argument; here it is any argv.
type ExecSpawner struct {
	Scope          *osx.Scope
	Path           string
	Args           []string
	Env            []string // nil: this process's environment
	Dir            string
	Stdout, Stderr io.Writer // the supervisor's own output (its log goes to files); nil discards
}

func (s *ExecSpawner) Spawn(ctx context.Context) (Proc, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.Scope.Start(osx.Cmd{Path: s.Path, Args: s.Args, Env: s.Env, Dir: s.Dir, Stdout: s.Stdout, Stderr: s.Stderr, Role: "supervisor"})
}

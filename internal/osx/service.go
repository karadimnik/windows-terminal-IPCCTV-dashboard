package osx

import (
	"context"
	"errors"
	"time"
)

// ServiceStart is a Windows service start type.
type ServiceStart string

const (
	StartAuto     ServiceStart = "auto"     // SERVICE_AUTO_START (delayed when ServiceConfig.Delayed)
	StartDemand   ServiceStart = "demand"   // SERVICE_DEMAND_START
	StartDisabled ServiceStart = "disabled" // SERVICE_DISABLED
)

// ServiceConfig is what holocron sets on its Windows service. BinaryPath is the full command line
// (quoted executable first); Account "" is LocalSystem. RestartDelay > 0 sets the recovery actions
// to restart after that delay (also on a non-crash failure: a non-zero exit code).
type ServiceConfig struct {
	Name, DisplayName, Description string
	BinaryPath                     string
	Account                        string
	Start                          ServiceStart
	Delayed                        bool
	RestartDelay                   time.Duration
}

// ServiceState is a service as the service control manager reports it.
type ServiceState struct {
	Exists  bool
	Config  ServiceConfig
	Running bool
	State   string // stopped | start-pending | stop-pending | running | ...
}

// ServiceManager is the seam over the Windows service control manager (advapi32): the real one is
// Services(), tests use FakeSCM. Off Windows every call returns ErrNoServiceManager.
type ServiceManager interface {
	Query(name string) (ServiceState, error) // Exists false (and no error) when there is no such service
	Create(c ServiceConfig) error
	Update(c ServiceConfig) error // replaces every field of an existing service
	SetStart(name string, start ServiceStart, delayed bool) error
	Delete(name string) error // marks it for deletion (a running service goes when it stops)
}

var (
	ErrNoServiceManager = errors.New("there is no Windows service control manager on this system")
	// ErrNotService: RunService was called in a process the service control manager did not start.
	ErrNotService = errors.New("not started by the Windows service control manager")
	// ErrAccessDenied: creating or changing a service needs an elevated (administrator) prompt.
	ErrAccessDenied = errors.New("access denied: installing a Windows service needs an elevated (Run as administrator) prompt")
)

// Services returns the service control manager of this system.
func Services() ServiceManager { return scm{} }

// RunService runs fn as the service name under the service control manager: it reports
// running, cancels fn's context on Stop, Shutdown or Preshutdown, and reports the exit code fn
// returns (non-zero = a service-specific failure, which triggers the recovery actions). It blocks
// until fn returns. ErrNotService when the process was not started by the manager.
func RunService(name string, fn func(ctx context.Context) int) error { return runService(name, fn) }

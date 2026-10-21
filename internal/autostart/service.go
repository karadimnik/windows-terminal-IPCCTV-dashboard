package autostart

import "errors"

// winService is the Windows service mode (built after the logon, systemd and launchd modes).
type winService struct{ env Env }

var errServiceNotBuilt = errors.New("--mode service is not built yet")

func (winService) entry(Spec) (Entry, error)             { return Entry{}, errServiceNotBuilt }
func (winService) current(string) (*Entry, State, error) { return nil, State{}, errServiceNotBuilt }
func (winService) apply(*Ops, Entry, *Entry) error       { return errServiceNotBuilt }
func (winService) remove(*Ops, Entry) error              { return errServiceNotBuilt }
func (winService) enable(*Ops, Entry, bool) error        { return errServiceNotBuilt }

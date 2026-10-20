package status

// State and StopKind values (see KNOWN-DIFFERENCES.md G1: the PowerShell
// writer's ValidateSet lacks window-closed, the schema and this package accept it).
const (
	StateRunning = "running"
	StateStopped = "stopped"

	StopKindNone         = ""
	StopKindRequested    = "requested"
	StopKindInterrupted  = "interrupted"
	StopKindUnexpected   = "unexpected"
	StopKindWindowClosed = "window-closed"
)

// CarryFrom carries the notices that outlive a supervisor restart from the
// previous document into d (the first full document of a new run):
//
//   - LastUnexpectedStop, re-emitted with its At text unchanged (the web app
//     dismisses the notice by comparing At byte for byte);
//   - LastWindowClose, also carried (decision D19; the PowerShell code drops it,
//     KNOWN-DIFFERENCES.md F1).
//
// A notice d already carries is newer and wins. With no previous document, or no
// such notice in it, LastUnexpectedStop is present as null (a full document
// always has the key) and LastWindowClose stays absent.
func (d *Document) CarryFrom(prev *Document) {
	if !d.LastUnexpectedStop.Present || d.LastUnexpectedStop.Stop == nil {
		d.LastUnexpectedStop = MaybeStop{Present: true}
		if prev != nil && prev.LastUnexpectedStop.Stop != nil {
			stop := *prev.LastUnexpectedStop.Stop
			d.LastUnexpectedStop.Stop = &stop
		}
	}
	if d.LastWindowClose == nil && prev != nil && prev.LastWindowClose != nil {
		wc := *prev.LastWindowClose
		d.LastWindowClose = &wc
	}
}

package prune

// Alert ids (Get-CamAlertEventIdList).
const (
	AlertDisabled = 120 // pruning disabled: the root or the marker refuses
	AlertBlocked  = 121 // pruning blocked: below the reserve with nothing deletable left
	AlertBreaker  = 122 // the age rule was held by the circuit breaker
)

// Alert is an event for the alerts package to send. Level is "Error" for all three.
type Alert struct {
	ID    int
	Level string
	Title string
	Body  string
}

// State is what the supervisor keeps between prune runs (New-CamPruneState), so each episode
// alerts once.
type State struct {
	RefusalAlerted    string // the PRUNING DISABLED message last alerted
	ClockRefused      bool
	BlockedAlerted    bool
	AgeBreakerTripped bool
	DeletedTotal      int
	FreedBytes        int64
	Blocked           bool
	LastMessage       string
}

// Episode says which episodes started or ended in a run, for the log.
type Episode struct {
	RefusalStarted bool
	ClockStarted   bool
	ClockCleared   bool
	BreakerStarted bool
	BreakerReset   bool
}

// Settle applies a finished run to the state and returns the alerts it raises, with the rules of
// Invoke-CamPrune: 120 once per distinct refusal text (re-armed by a passing run); no alert for a
// clock refusal; 122 once per breaker episode (re-armed by a real run without a breach); 121 once
// per blocked episode (re-armed by a real run that was not blocked). A dry run never alerts.
func (s *State) Settle(d Decision, dryRun bool) ([]Alert, Episode) {
	var alerts []Alert
	var ep Episode
	if d.Refusal != nil && d.Refusal.Gate != GateClock {
		if !dryRun {
			if s.RefusalAlerted != d.Message {
				alerts = append(alerts, Alert{ID: AlertDisabled, Level: "Error", Title: "pruning is disabled - the disk will fill", Body: d.Refusal.Reason})
				s.RefusalAlerted = d.Message
				ep.RefusalStarted = true
			}
			s.Blocked = true
			s.LastMessage = d.Message
		}
		return alerts, ep
	}
	s.RefusalAlerted = ""

	if d.Refusal != nil { // the clock gate
		if !dryRun {
			ep.ClockStarted = !s.ClockRefused
			s.ClockRefused = true
			s.Blocked = true
			s.LastMessage = d.Message
		}
		return alerts, ep
	}
	if s.ClockRefused {
		s.ClockRefused = false
		ep.ClockCleared = true
	}

	if d.BreakerHeld && !dryRun && !s.AgeBreakerTripped {
		alerts = append(alerts, Alert{ID: AlertBreaker, Level: "Error", Title: "the age rule was held - MaxRetentionDays would delete most of the archive", Body: heldLine(d.Breaker)})
		s.AgeBreakerTripped = true
		ep.BreakerStarted = true
	}
	if !d.BreakerHeld && s.AgeBreakerTripped && !dryRun {
		s.AgeBreakerTripped = false
		ep.BreakerReset = true
	}

	if dryRun {
		return alerts, ep
	}
	if d.Blocked && !s.BlockedAlerted {
		alerts = append(alerts, Alert{ID: AlertBlocked, Level: "Error", Title: "pruning is blocked - the disk is filling", Body: d.Message})
		s.BlockedAlerted = true
	}
	s.DeletedTotal += len(d.Deletions)
	s.FreedBytes += d.FreedBytes
	s.Blocked = d.Blocked
	if !d.Blocked {
		s.BlockedAlerted = false
	}
	if d.Message != "" {
		s.LastMessage = d.Message
	}
	return alerts, ep
}

func heldLine(breach string) string {
	return "PRUNE AGE RULE HELD by the circuit breaker: " + breach + ". Nothing is deleted by age this run; the free-space watermark still runs and recording continues. If MaxRetentionDays is mistyped, correct it; if it is meant, lower it in steps or raise MaxAgePruneFraction (1.0 switches the brake off)."
}

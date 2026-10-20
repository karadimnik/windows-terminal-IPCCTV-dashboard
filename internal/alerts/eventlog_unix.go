//go:build !windows

package alerts

import (
	"fmt"
	"log/syslog"
)

type syslogWriter struct{ w *syslog.Writer }

// openOSEventLog connects to the local syslog daemon (journald listens on /dev/log). The tag is
// "holocron"; the entry is prefixed "IPCCTV: ".
var openOSEventLog = func() (EventWriter, error) {
	w, err := syslog.New(syslog.LOG_USER|syslog.LOG_NOTICE, "holocron")
	if err != nil {
		return nil, fmt.Errorf("syslog: %w", err)
	}
	return syslogWriter{w}, nil
}

func (s syslogWriter) Write(id int, sev Severity, msg string) error {
	msg = fmt.Sprintf("%s [alert %d]", msg, id)
	switch sev {
	case Error:
		return s.w.Err(msg)
	case Warning:
		return s.w.Warning(msg)
	}
	return s.w.Info(msg)
}

func (s syslogWriter) Close() error { return s.w.Close() }

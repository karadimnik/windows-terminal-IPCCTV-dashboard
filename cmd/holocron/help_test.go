package main

import (
	"regexp"
	"strings"
	"testing"
)

var usageFlag = regexp.MustCompile(`(?:^|[\s\[|])(--?[A-Za-z][A-Za-z0-9-]*)`)

// Every registered command answers --help, -h and `help <command>` with exit 0, its name, its
// description and each flag its usage line declares (all derived from the registry), and runs nothing.
func TestEveryCommandHasHelp(t *testing.T) {
	for _, c := range commands {
		words := strings.Fields(c.path)
		var flags []string
		for _, m := range usageFlag.FindAllStringSubmatch(c.usage, -1) {
			flags = append(flags, m[1])
		}
		for _, form := range [][]string{append(append([]string{}, words...), "--help"), append(append([]string{}, words...), "-h"), append([]string{"help"}, words...), append([]string{"--help"}, words...)} {
			r := newRig(t)
			out, errw, code := r.run(form...)
			if code != 0 || errw != "" {
				t.Errorf("%v: exit %d, stderr %q", form, code, errw)
			}
			for _, want := range append([]string{"holocron " + c.path, c.about, "usage:"}, flags...) {
				if !strings.Contains(out, want) {
					t.Errorf("%v: output lacks %q:\n%s", form, want, out)
				}
			}
			if len(r.ctl.calls) != 0 || r.log() != "" && c.path != "web" {
				t.Errorf("%v: the command ran (ctl %v)", form, r.ctl.calls)
			}
		}
	}
}

func TestHelpOfAnUnknownCommandIsAUsageError(t *testing.T) {
	r := newRig(t)
	for _, args := range [][]string{{"help", "bogus"}, {"bogus", "--help"}, {"--help", "bogus"}, {"help", "cam", "bogus"}, {"help", "up", "extra"}} {
		if _, errw, code := r.run(args...); code != 2 || !strings.Contains(errw, "unknown command") {
			t.Errorf("%v: exit %d %q", args, code, errw)
		}
	}
}

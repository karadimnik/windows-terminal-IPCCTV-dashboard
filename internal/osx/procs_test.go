package osx

import (
	"os"
	"runtime"
	"testing"
)

// The list holds this process with its parent.
func TestListProcsHoldsThisProcess(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip("not implemented on " + runtime.GOOS)
	}
	ps, err := ListProcs()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.Pid == os.Getpid() {
			if p.PPid != os.Getppid() || p.Name == "" {
				t.Errorf("%+v, parent %d", p, os.Getppid())
			}
			return
		}
	}
	t.Errorf("pid %d not among %d processes", os.Getpid(), len(ps))
}

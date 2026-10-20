// SP3: do the children die with the supervisor? One binary, several modes (see usage). A spike, not product code.
//
//	sp3 supervise -dir D -tag T -n 3 [-kind direct|grand|leafbreak] [-short K -shortms MS] [-reg]
//	    [windows: -assign suspended|after|joblist|none -gap MS -breakaway -nojob]
//	    [linux:   -mode locked|unlocked|lockexit|lockexitunlock|none -wrapper]
//	sp3 leaf -dir D [-life MS] [-spawn] [-breakaway]   a child that optionally starts a grandchild
//	sp3 reap -reg F                                    what a NEW supervisor does at start
//	sp3 regfake -reg F -pid P -kind wrongstart|wrongimage|dead
//	sp3 wrap -- cmd args...                            stdin-pipe watcher shim (kills cmd when stdin reaches EOF)
//	sp3 jobwrap -dir D [-kill] -- exe args...          windows: run exe inside an outer job
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

type entry struct {
	Pid   int    `json:"pid"`
	Start string `json:"start"`
	Image string `json:"image"`
	Role  string `json:"role"`
}

type registry struct {
	Supervisor int     `json:"supervisor"`
	Children   []entry `json:"children"`
}

// opts carries both the common flags and the per-OS state (job handle, group id, open pipes).
type opts struct {
	dir, tag, kind, assign, mode string
	n, short, shortMs, gap       int
	reg, breakaway, nojob        bool
	wrapper                      bool
	job                          uintptr
	pgid                         int
	pipes                        []io.Closer
	logf                         *os.File
}

func (o *opts) log(format string, a ...any) {
	fmt.Fprintf(o.logf, "%d %s\n", time.Now().UnixMilli(), fmt.Sprintf(format, a...))
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: sp3 supervise|leaf|reap|regfake|wrap|jobwrap ...")
		os.Exit(2)
	}
	args := os.Args[2:]
	switch os.Args[1] {
	case "supervise":
		cmdSupervise(args)
	case "leaf":
		cmdLeaf(args)
	case "reap":
		cmdReap(args)
	case "regfake":
		cmdRegFake(args)
	case "wrap":
		cmdWrap(args)
	case "jobwrap":
		cmdJobWrap(args)
	default:
		fmt.Println("unknown mode")
		os.Exit(2)
	}
}

func cmdSupervise(args []string) {
	o := &opts{}
	fs := flag.NewFlagSet("supervise", flag.ExitOnError)
	fs.StringVar(&o.dir, "dir", ".", "state dir")
	fs.StringVar(&o.tag, "tag", "run", "tag")
	fs.IntVar(&o.n, "n", 3, "children")
	fs.StringVar(&o.kind, "kind", "direct", "direct|grand|leafbreak")
	fs.IntVar(&o.short, "short", 0, "how many of the children are short-lived leaves")
	fs.IntVar(&o.shortMs, "shortms", 1000, "lifetime of a short child")
	fs.BoolVar(&o.reg, "reg", false, "write the registry")
	fs.StringVar(&o.assign, "assign", "suspended", "windows: suspended|after|joblist|none")
	fs.IntVar(&o.gap, "gap", 0, "windows: artificial ms between CreateProcess and the assignment")
	fs.BoolVar(&o.breakaway, "breakaway", false, "windows: job allows breakaway")
	fs.BoolVar(&o.nojob, "nojob", false, "windows: no job at all (control)")
	fs.StringVar(&o.mode, "mode", "locked", "linux: locked|unlocked|lockexit|lockexitunlock|none")
	fs.BoolVar(&o.wrapper, "wrapper", false, "start children through the stdin-pipe shim")
	fs.Parse(args)
	var err error
	o.logf, err = os.OpenFile(filepath.Join(o.dir, o.tag+".sup.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		panic(err)
	}
	self, _ := os.Executable()
	o.log("supervisor start pid=%d", os.Getpid())
	platInit(o)

	var roles []string
	var pids []int
	var startMs []int64
	platRun(o, func() {
		for i := 0; i < o.n; i++ {
			var argv []string
			role := o.kind
			switch {
			case i < o.short:
				argv, role = []string{self, "leaf", "-dir", o.dir, "-life", fmt.Sprint(o.shortMs)}, "short"
			case o.kind == "grand":
				argv = []string{self, "leaf", "-dir", o.dir, "-spawn"}
			case o.kind == "leafbreak":
				argv = []string{self, "leaf", "-dir", o.dir, "-spawn", "-breakaway"}
			default:
				argv = longRunner()
			}
			t := time.Now().UnixMilli()
			pid, err := platStart(o, argv)
			if err != nil {
				o.log("start child %d failed: %v", i, err)
				continue
			}
			o.log("started child %s pid=%d", role, pid)
			roles, pids, startMs = append(roles, role), append(pids, pid), append(startMs, t)
		}
	})
	state := map[string]any{"supervisor": os.Getpid(), "children": pids, "roles": roles, "start_ms": startMs}
	b, _ := json.Marshal(state)
	os.WriteFile(filepath.Join(o.dir, o.tag+".state.json"), b, 0o644)
	if o.reg {
		r := registry{Supervisor: os.Getpid()}
		for i, p := range pids {
			s, img, err := procInfo(p)
			if err != nil {
				o.log("registry: child %d: %v", p, err)
				continue
			}
			r.Children = append(r.Children, entry{p, s, img, roles[i]})
		}
		writeJSON(filepath.Join(o.dir, o.tag+".children.json"), r)
	}
	o.log("ready")
	platWait(o) // returns after an orderly stop request; hard kills never return
}

func writeJSON(p string, v any) {
	b, _ := json.MarshalIndent(v, "", " ")
	os.WriteFile(p, b, 0o644)
}

// cmdLeaf is a child: optionally starts a grandchild at once (to probe the gap between CreateProcess and job assignment), then lives.
func cmdLeaf(args []string) {
	fs := flag.NewFlagSet("leaf", flag.ExitOnError)
	dir := fs.String("dir", ".", "")
	life := fs.Int("life", 0, "ms to live (0 = until killed)")
	spawn := fs.Bool("spawn", false, "")
	brk := fs.Bool("breakaway", false, "")
	fs.Parse(args)
	lf, _ := os.OpenFile(filepath.Join(*dir, fmt.Sprintf("leaf-%d.log", os.Getpid())), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	lg := func(f string, a ...any) { fmt.Fprintf(lf, "%d %s\n", time.Now().UnixMilli(), fmt.Sprintf(f, a...)) }
	lg("leaf start pid=%d", os.Getpid())
	if *spawn {
		pid, err := spawnLong(*brk)
		lg("spawn grandchild breakaway=%v pid=%d err=%v", *brk, pid, err)
	}
	if *life > 0 {
		time.Sleep(time.Duration(*life) * time.Millisecond)
		lg("leaf exit (life over)")
		return
	}
	for {
		time.Sleep(time.Hour)
	}
}

func cmdReap(args []string) {
	fs := flag.NewFlagSet("reap", flag.ExitOnError)
	regp := fs.String("reg", "children.json", "")
	fs.Parse(args)
	var r registry
	b, err := os.ReadFile(*regp)
	if err != nil {
		panic(err)
	}
	json.Unmarshal(b, &r)
	for _, e := range r.Children {
		s, img, err := procInfo(e.Pid)
		verdict := ""
		switch {
		case err != nil:
			verdict = "skip: not running"
		case s != e.Start:
			verdict = "skip: start time differs (pid reused)"
		case !sameImage(img, e.Image):
			verdict = "skip: image path differs"
		default:
			if err := killPid(e.Pid); err != nil {
				verdict = "kill failed: " + err.Error()
				break
			}
			verdict = "KILLED (pid alive, same start time, same image)"
			for i := 0; i < 40; i++ {
				if _, _, err := procInfo(e.Pid); err != nil {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
		}
		fmt.Printf("reap pid=%d role=%s: %s\n", e.Pid, e.Role, verdict)
	}
}

func sameImage(a, b string) bool { return platCaseFold(a, b) }

func cmdRegFake(args []string) {
	fs := flag.NewFlagSet("regfake", flag.ExitOnError)
	regp := fs.String("reg", "children.json", "")
	pid := fs.Int("pid", 0, "")
	kind := fs.String("kind", "wrongstart", "")
	fs.Parse(args)
	var r registry
	if b, err := os.ReadFile(*regp); err == nil {
		json.Unmarshal(b, &r)
	}
	s, img, err := procInfo(*pid)
	if err != nil {
		panic(err)
	}
	switch *kind {
	case "wrongstart":
		s = "1" + s // the recorded process "started" at another time: a different incarnation of the pid
	case "wrongimage":
		img = img + ".other"
	case "dead":
		*pid = 4000000 // not a live pid (above the default pid ranges)
	}
	r.Children = append(r.Children, entry{*pid, s, img, "fake-" + *kind})
	writeJSON(*regp, r)
}

// cmdWrap: the stdin-pipe liveness shim. The supervisor holds the write end; when it dies the kernel closes it.
func cmdWrap(args []string) {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	cmd := exec.Command(args[0], args[1:]...)
	platWrapAttr(cmd)
	if err := cmd.Start(); err != nil {
		panic(err)
	}
	go func() {
		io.Copy(io.Discard, os.Stdin)
		platKillTree(cmd.Process.Pid)
		os.Exit(0)
	}()
	cmd.Wait()
}

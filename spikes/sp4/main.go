// sp4 tests the "open segment" rule of HLD decision 1: the newest file whose name matches the strict
// segment pattern, with an mtime within SegmentSeconds+60 s of now.
//
//	sp4 pick  -dir D -safe Cam -seg 5            one-shot: print candidates and the rule's pick
//	sp4 watch -dir D -safe Cam -seg 5 -every 2s -for 120s
//	                                              poll; per line: pick, age, ground truth (held open?),
//	                                              size and mtime from the directory listing vs from stat
//	sp4 sim   -dir D                              build synthetic files (set mtimes) for clock-step and DST cases
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type cand struct {
	name  string
	mtime time.Time
}

func strict(safe string) *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(safe) + `_\d{4}\d{2}\d{2}_\d{2}\d{2}\d{2}[+-]\d{4}\.mkv$`)
}

// pick returns the rule's choice: among strict-pattern names, the greatest mtime (ties: greatest name),
// accepted only when its mtime is within window of now. Also returns the name-sorted-last candidate for comparison.
func pick(dir, safe string, window time.Duration, now time.Time) (chosen, byName string, all []cand) {
	re := strict(safe)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.IsDir() || !re.MatchString(e.Name()) {
			continue
		}
		fi, err := os.Stat(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		all = append(all, cand{e.Name(), fi.ModTime()})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].name < all[j].name })
	if len(all) == 0 {
		return
	}
	byName = all[len(all)-1].name
	best := all[0]
	for _, c := range all {
		if !c.mtime.Before(best.mtime) {
			best = c
		}
	}
	if d := now.Sub(best.mtime); d <= window {
		chosen = best.name
	}
	return
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: sp4 pick|watch|sim ...")
		os.Exit(2)
	}
	cmd := os.Args[1]
	fl := flag.NewFlagSet(cmd, flag.ExitOnError)
	dir := fl.String("dir", ".", "camera folder")
	safe := fl.String("safe", "Cam", "safe name")
	seg := fl.Int("seg", 5, "SegmentSeconds")
	every := fl.Duration("every", 2*time.Second, "poll interval")
	dur := fl.Duration("for", 60*time.Second, "watch duration")
	fl.Parse(os.Args[2:])
	window := time.Duration(*seg+60) * time.Second
	switch cmd {
	case "pick":
		c, n, all := pick(*dir, *safe, window, time.Now())
		for _, x := range all {
			fmt.Printf("  %s mtime=%s age=%.1fs\n", x.name, x.mtime.Format("15:04:05.000"), time.Since(x.mtime).Seconds())
		}
		fmt.Printf("pick=%q byNameLast=%q\n", c, n)
	case "watch":
		end := time.Now().Add(*dur)
		for time.Now().Before(end) {
			now := time.Now()
			c, n, _ := pick(*dir, *safe, window, now)
			line := fmt.Sprintf("%s pick=%-30q", now.Format("15:04:05"), c)
			if c == "" {
				line += " (none: aged out or no files)"
			} else {
				line += describe(*dir, c, now)
			}
			if n != c && c != "" {
				line += " NAME-SORT-DIFFERS:" + n
			}
			fmt.Println(line)
			time.Sleep(*every)
		}
	case "sim":
		sim(*dir, *safe)
	}
}

// describe adds ground truth and the two views of size/mtime.
func describe(dir, name string, now time.Time) string {
	p := filepath.Join(dir, name)
	held := heldOpen(p)
	ents, _ := os.ReadDir(dir)
	var ls string
	for _, e := range ents {
		if e.Name() == name {
			if fi, err := e.Info(); err == nil {
				ls = fmt.Sprintf("listing(size=%d,age=%.1fs)", fi.Size(), now.Sub(fi.ModTime()).Seconds())
			}
		}
	}
	st := ""
	if fi, err := os.Stat(p); err == nil {
		st = fmt.Sprintf("stat(size=%d,age=%.1fs)", fi.Size(), now.Sub(fi.ModTime()).Seconds())
	}
	return fmt.Sprintf(" heldOpen=%v %s %s%s", held, ls, st, birth(p))
}

func sim(dir, safe string) {
	must := func(err error) {
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
	}
	mk := func(sub, name string, mtime time.Time) {
		d := filepath.Join(dir, sub)
		must(os.MkdirAll(d, 0o755))
		p := filepath.Join(d, name)
		must(os.WriteFile(p, []byte("x"), 0o644))
		must(os.Chtimes(p, mtime, mtime))
	}
	now := time.Now()
	// healthy: three files, the last written 2 s ago
	mk("healthy", safe+"_20261017_120000+0300.mkv", now.Add(-12*time.Second))
	mk("healthy", safe+"_20261017_120005+0300.mkv", now.Add(-7*time.Second))
	mk("healthy", safe+"_20261017_120010+0300.mkv", now.Add(-2*time.Second))
	// clock stepped FORWARD one hour after the last write: everything looks old
	mk("fwd1h", safe+"_20261017_120000+0300.mkv", now.Add(-3600*time.Second-7*time.Second))
	mk("fwd1h", safe+"_20261017_120005+0300.mkv", now.Add(-3600*time.Second-2*time.Second))
	// clock stepped BACKWARD one hour after the last write: mtimes are in the future
	mk("back1h", safe+"_20261017_120000+0300.mkv", now.Add(3600*time.Second-7*time.Second))
	mk("back1h", safe+"_20261017_120005+0300.mkv", now.Add(3600*time.Second-2*time.Second))
	// DST fall-back: 03:50 +0300 is older than 03:10 +0200 although it sorts later
	mk("dst", safe+"_20261025_035000+0300.mkv", now.Add(-12*time.Second))
	mk("dst", safe+"_20261025_035500+0300.mkv", now.Add(-7*time.Second))
	mk("dst", safe+"_20261025_030000+0200.mkv", now.Add(-2*time.Second))
	// foreign names never win
	mk("foreign", safe+"_20261017_120000+0300.mkv", now.Add(-30*time.Second))
	mk("foreign", safe+"_20261017_120000+0300.mkv.tmp", now.Add(-1*time.Second))
	mk("foreign", "Other_20261017_120000+0300.mkv", now.Add(-1*time.Second))
	mk("foreign", strings.ToLower(safe)+"_20261017_120005+0300.mkv", now.Add(-1*time.Second))
	fmt.Println("built")
}

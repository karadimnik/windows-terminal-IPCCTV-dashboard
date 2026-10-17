// sp6 tests atomic replace of a status-like JSON file with concurrent readers (spike SP6).
//
//	sp6 write -path P [-every 100ms] [-for 10s] [-retries 0] [-retrydelay 100ms] [-selfread]
//	sp6 read  -path P [-for 10s] [-open go|sd] [-hold 0]
//
// write: temp file in the same directory, then os.Rename over P, every -every. Counts rename failures.
// read: tight loop (or open-hold-close with -hold) and counts good reads, partial/invalid JSON, errors by kind.
// -open go uses os.Open; -open sd opens with FILE_SHARE_READ|WRITE|DELETE on Windows.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

type doc struct {
	Schema int
	Seq    int
	At     string
	Pad    string
}

type stats struct {
	mu                          sync.Mutex
	ok, partial, regress, total int
	errs                        map[string]int
	last                        int
}

func (s *stats) err(e error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.errs == nil {
		s.errs = map[string]int{}
	}
	s.errs[kind(e)]++
}

func kind(e error) string {
	var en syscall.Errno
	if errors.As(e, &en) {
		switch en {
		case 5:
			return "ACCESS_DENIED(5)"
		case 32:
			return "SHARING_VIOLATION(32)"
		case 2, 3:
			return "NOT_FOUND"
		}
		return fmt.Sprintf("errno%d", int(en))
	}
	if errors.Is(e, fs.ErrNotExist) {
		return "NOT_FOUND"
	}
	return strings.ReplaceAll(e.Error(), "\n", " ")
}

func (s *stats) read(b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total++
	var d doc
	if err := json.Unmarshal(b, &d); err != nil {
		s.partial++
		return
	}
	s.ok++
	if d.Seq < s.last {
		s.regress++
	}
	s.last = d.Seq
}

func (s *stats) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ks []string
	for k, v := range s.errs {
		ks = append(ks, fmt.Sprintf("%s=%d", k, v))
	}
	sort.Strings(ks)
	return fmt.Sprintf("reads=%d ok=%d partial_or_invalid=%d seq_regress=%d errors=[%s]", s.total, s.ok, s.partial, s.regress, strings.Join(ks, " "))
}

func readOnce(path, mode string, hold time.Duration, st *stats) {
	f, err := openRead(path, mode)
	if err != nil {
		st.err(err)
		return
	}
	b, err := io.ReadAll(f)
	if err != nil {
		st.err(err)
	} else {
		st.read(b)
	}
	if hold > 0 {
		time.Sleep(hold)
	}
	f.Close()
}

func reader(path, mode string, hold time.Duration, until time.Time, st *stats) {
	for time.Now().Before(until) {
		readOnce(path, mode, hold, st)
	}
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: sp6 write|read ...")
		os.Exit(2)
	}
	cmd := os.Args[1]
	fl := flag.NewFlagSet(cmd, flag.ExitOnError)
	path := fl.String("path", "status.json", "file")
	every := fl.Duration("every", 100*time.Millisecond, "write interval")
	dur := fl.Duration("for", 10*time.Second, "run time")
	retries := fl.Int("retries", 0, "rename retries after the first attempt")
	retryDelay := fl.Duration("retrydelay", 100*time.Millisecond, "delay between retries")
	selfread := fl.Bool("selfread", false, "also run a tight reader goroutine in the writer process")
	open := fl.String("open", "go", "go|sd")
	hold := fl.Duration("hold", 0, "reader keeps the file open this long per read")
	fl.Parse(os.Args[2:])
	until := time.Now().Add(*dur)
	switch cmd {
	case "write":
		var st stats
		var wg sync.WaitGroup
		if *selfread {
			wg.Add(1)
			go func() { defer wg.Done(); reader(*path, "go", 0, until, &st) }()
		}
		writes, fails, failedAfterRetry, retried := 0, 0, 0, 0
		errs := map[string]int{}
		pad := strings.Repeat("x", 8000)
		for seq := 1; time.Now().Before(until); seq++ {
			b, _ := json.MarshalIndent(doc{1, seq, time.Now().Format(time.RFC3339Nano), pad}, "", "  ")
			tmp := *path + ".tmp"
			if err := os.WriteFile(tmp, b, 0o644); err != nil {
				errs["write-tmp:"+kind(err)]++
				continue
			}
			var err error
			for try := 0; try <= *retries; try++ {
				if err = os.Rename(tmp, *path); err == nil {
					if try > 0 {
						retried++
					}
					break
				}
				fails++
				errs[kind(err)]++
				time.Sleep(*retryDelay)
			}
			if err != nil {
				failedAfterRetry++
			}
			writes++
			time.Sleep(*every)
		}
		wg.Wait()
		var ks []string
		for k, v := range errs {
			ks = append(ks, fmt.Sprintf("%s=%d", k, v))
		}
		sort.Strings(ks)
		fmt.Printf("WRITER cycles=%d rename_failures=%d saved_by_retry=%d failed_after_all_retries=%d errors=[%s]\n", writes, fails, retried, failedAfterRetry, strings.Join(ks, " "))
		if *selfread {
			fmt.Println("GOROUTINE-READER", st.String())
		}
	case "read":
		var st stats
		reader(*path, *open, *hold, until, &st)
		fmt.Printf("READER open=%s hold=%s %s\n", *open, *hold, st.String())
	}
}

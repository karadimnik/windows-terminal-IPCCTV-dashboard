package prune

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"holocron/internal/archive"
	"holocron/internal/config"
	"holocron/internal/osx"
)

// The ten prune* checks of tests/cam.tests.ps1, ported as Go tests with the same fixtures and
// assertions (prunelive reads the real archive and is not ported; pruneowner and pruneloop keep
// their prune assertions, the supervisor parts belong to the supervisor's tests).

const day = 24 * time.Hour

// symlink makes a link or reports that this account may not (Windows without the privilege).
func (r *rig) symlink(target, link string) bool {
	r.t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		r.t.Logf("no symbolic link %s: %v", link, err)
		return false
	}
	return true
}

func TestPruneMatrix(t *testing.T) {
	r := newRig(t)
	r.rules.MaxRetentionDays, r.rules.MaxAgePruneFraction = ptr(1), 1.0
	now := r.now
	old := now.Add(-10 * day)
	fe := filepath.Join(r.root, "Front_Entrance")
	ws := filepath.Join(r.root, "Workshop")
	outside := filepath.Join(r.dir, "outside")
	sibling := r.root + "_old"

	expected := []string{
		r.seg("Front_Entrance", now.Add(-10*day)), r.seg("Front_Entrance", now.Add(-9*day)),
		r.seg("Front_Entrance", now.Add(-8*day)), r.seg("Workshop", now.Add(-7*day)),
	}
	r.seg("Front_Entrance", now.Add(-2*time.Minute))
	r.seg("Workshop", now.Add(-2*time.Minute))
	r.claim()

	for _, n := range []string{"notes.txt", "desktop.ini", "Front_Entrance_export.mkv"} {
		r.put(filepath.Join(fe, n), 32, old)
	}
	r.put(filepath.Join(fe, "empty.dat"), 0, old)
	r.segExt("Front_Entrance", "Workshop", old, ".mkv")
	r.segExt("Front_Entrance", "Front_Entrance", old.Add(time.Hour), ".mp4")
	r.segExt("Front_Entrance", "Front_Entrance", old.Add(2*time.Hour), ".mkv.tmp")
	r.put(filepath.Join(ws, "Thumbs.db"), 32, old)
	for _, own := range []string{"_state", "_logs", "_selftest"} {
		r.segExt(own, "Front_Entrance", old, ".mkv")
	}
	r.put(filepath.Join(sibling, "Front_Entrance", segName("Front_Entrance", old, ".mkv")), 64, old)
	r.put(filepath.Join(outside, "Linked", segName("Linked", old, ".mkv")), 64, old)
	r.put(filepath.Join(outside, "Linked", segName("Linked", old.Add(time.Hour), ".mkv")), 64, old.Add(time.Hour))
	topLink := r.symlink(filepath.Join(outside, "Linked"), filepath.Join(r.root, "Linked"))
	r.put(filepath.Join(outside, "clips", segName("Front_Entrance", old, ".mkv")), 64, old)
	r.symlink(filepath.Join(outside, "clips"), filepath.Join(fe, "clips"))
	target := r.put(filepath.Join(outside, "target", segName("Front_Entrance", now.Add(-11*day), ".mkv")), 64, now.Add(-11*day))
	linkPath := filepath.Join(fe, filepath.Base(target))
	fileLink := r.symlink(target, linkPath)

	before := r.manifest()
	res := r.run(false)
	removed, changed, added := diffManifest(before, r.manifest())
	if !eqList(removed, sorted(expected)) || len(changed)+len(added) > 0 {
		t.Fatalf("removed %v changed %v added %v, want exactly %v", base(removed), changed, added, base(expected))
	}
	if len(res.Foreign) != 2 || res.Foreign[0].Dir != fe || res.Foreign[0].Count != 7 || res.Foreign[1].Dir != ws || res.Foreign[1].Count != 1 {
		t.Fatalf("foreign counts %+v, want 7 in Front_Entrance and 1 in Workshop", res.Foreign)
	}
	rejected := strings.Join(res.Rejected, "\n")
	if fileLink && !strings.Contains(rejected, linkPath+" (reparse point)") {
		t.Fatalf("the file link was not refused by name: %v", res.Rejected)
	}
	if topLink && !strings.Contains(rejected, filepath.Join(r.root, "Linked")+" (reparse point") {
		t.Fatalf("the top-level link was not refused: %v", res.Rejected)
	}
	if !strings.HasPrefix(res.StopLine, "prune stop: age rule") {
		t.Fatalf("stop line %q", res.StopLine)
	}
	if n := r.log.count(func(x rec) bool {
		return x.Attrs["action"] == "prune.delete" && x.Attrs["result"] == "ok" && x.Level.String() == "INFO" && x.Attrs["actor"] == "supervisor" &&
			x.Attrs["file"] != "" && strings.HasSuffix(x.Attrs["target"], x.Attrs["file"]) && x.Attrs["bytes"] != "" && x.Attrs["reason"] == ReasonAge
	}); n != 4 {
		t.Fatalf("%d INFO prune.delete records with target, file, bytes and reason, want 4", n)
	}
}

func (r *rig) rootSeed() {
	now := r.now
	r.seg("Front_Entrance", now.Add(-5*day))
	r.seg("Front_Entrance", now.Add(-4*day))
	r.seg("Front_Entrance", now.Add(-3*time.Minute), 0)
	r.seg("Front_Entrance", now.Add(-time.Minute))
}

func TestPruneRoots(t *testing.T) {
	// (b) a drive root and the filesystem root, judged with the real per-OS rules (lexically).
	for _, c := range []struct {
		goos, root, want string
	}{
		{"windows", `Z:\`, "is the root of the volume"},
		{"windows", `\\server\share`, "is the root of the share"},
		{"linux", "/", "is the root of the filesystem"},
	} {
		in := Inputs{Root: c.root, RootProblem: archive.CheckRoot(c.root, &osx.Fake{OS: c.goos}), Rules: Rules{Container: ".mkv"}}
		var st State
		var alerts int
		for i := 0; i < 2; i++ {
			d := Plan(in)
			if d.Refusal == nil || d.Refusal.Gate != GateRoot || !strings.Contains(d.Refusal.Reason, c.want) || !d.Blocked || len(d.Deletions) > 0 {
				t.Fatalf("%s: %+v", c.root, d.Refusal)
			}
			a, _ := st.Settle(d, false)
			alerts += len(a)
		}
		if alerts != 1 || !strings.HasPrefix(st.LastMessage, "PRUNING DISABLED") {
			t.Fatalf("%s: %d alerts over two cycles, last message %q", c.root, alerts, st.LastMessage)
		}
	}

	scenario := func(t *testing.T, r *rig, want string) {
		before := r.manifest()
		first, second := r.run(false), r.run(false)
		removed, changed, added := diffManifest(before, r.manifest())
		alerts := len(first.Alerts) + len(second.Alerts)
		if !first.Blocked || !second.Blocked || len(first.Deletions)+len(second.Deletions) > 0 || len(removed)+len(changed)+len(added) > 0 ||
			alerts != 1 || first.Alerts[0].ID != AlertDisabled || !strings.Contains(first.Refusal.Reason, want) || !r.st.Blocked ||
			!strings.HasPrefix(r.st.LastMessage, "PRUNING DISABLED") {
			t.Fatalf("blocked %v/%v, deleted %d, files changed %d, alerts %d, refusal %+v", first.Blocked, second.Blocked,
				len(first.Deletions)+len(second.Deletions), len(removed)+len(changed)+len(added), alerts, first.Refusal)
		}
	}

	t.Run("profile-like", func(t *testing.T) {
		r := newRig(t)
		r.rules.MaxRetentionDays, r.rules.MaxAgePruneFraction = ptr(1), 1.0
		r.rootSeed()
		for _, leaf := range []string{"Desktop/todo.txt", "Documents/report.docx", "AppData/Local/settings.dat", "NTUSER.DAT"} {
			r.put(filepath.Join(r.root, filepath.FromSlash(leaf)), 16, r.now.Add(-30*day))
		}
		claim, err := archive.ClaimRoot(r.root, osx.Current(), ".mkv", r.now)
		if err != nil || claim.Created || !strings.Contains(claim.Problem, "NTUSER.DAT") {
			t.Fatalf("claim %+v %v", claim, err)
		}
		scenario(t, r, "cannot claim the root")
	})
	t.Run("no-marker", func(t *testing.T) {
		r := newRig(t)
		r.rules.MaxRetentionDays, r.rules.MaxAgePruneFraction = ptr(1), 1.0
		r.rootSeed()
		scenario(t, r, "restart the recorder and it claims it")
		before := r.manifest()
		preview := r.run(true)
		removed, changed, added := diffManifest(before, r.manifest())
		if preview.Refusal != nil || len(preview.Deletions) != 2 || len(removed)+len(changed)+len(added) > 0 || exists(archive.MarkerPath(r.root)) {
			t.Fatalf("dry run: refusal %+v, would delete %d, files changed %d", preview.Refusal, len(preview.Deletions), len(removed)+len(changed)+len(added))
		}
	})
	t.Run("foreign-top-level", func(t *testing.T) {
		r := newRig(t)
		r.rules.MaxRetentionDays, r.rules.MaxAgePruneFraction = ptr(1), 1.0
		r.rootSeed()
		r.put(filepath.Join(r.root, "Photos", "holiday.jpg"), 3, r.now)
		r.put(filepath.Join(r.root, "readme.txt"), 3, r.now)
		claim, _ := archive.ClaimRoot(r.root, osx.Current(), ".mkv", r.now)
		if claim.Created || !strings.Contains(claim.Problem, "Photos") || !strings.Contains(claim.Problem, "readme.txt") {
			t.Fatalf("claim %+v", claim)
		}
		scenario(t, r, "cannot claim the root")
	})
}

func TestPruneAge(t *testing.T) {
	r := newRig(t)
	r.rules.MaxRetentionDays = ptr(1)
	now := r.now
	a5 := r.seg("Front_Entrance", now.Add(-5*day))
	b45 := r.seg("Workshop", now.Add(-108*time.Hour))
	a4 := r.seg("Front_Entrance", now.Add(-4*day))
	a3 := r.seg("Front_Entrance", now.Add(-3*day))
	a2h := r.seg("Front_Entrance", now.Add(-2*time.Hour))
	aNew := r.seg("Front_Entrance", now.Add(-time.Minute))
	bNew := r.seg("Workshop", now.Add(-84*time.Hour))
	// Named (created) before Workshop's newest but WRITTEN after it: old enough for the cutoff,
	// yet not strictly older than its folder's newest.
	bHeld := r.seg("Workshop", now.Add(-6*day))
	r.touch(bHeld, now.Add(-81*time.Hour-36*time.Minute))
	expected := []string{a5, b45, a4, a3}
	r.claim()

	before := r.manifest()
	dry := r.run(true)
	if rm, ch, ad := diffManifest(before, r.manifest()); len(rm)+len(ch)+len(ad) > 0 {
		t.Fatal("the dry run changed the filesystem")
	}
	real := r.run(false)
	if !eqList(paths(real.Deletions), expected) {
		t.Fatalf("deleted %v, want %v", base(paths(real.Deletions)), base(expected))
	}
	if !eqList(paths(dry.Deletions), paths(real.Deletions)) {
		t.Fatalf("dry run listed %v", base(paths(dry.Deletions)))
	}
	removed, changed, added := diffManifest(before, r.manifest())
	if !eqList(removed, sorted(expected)) || len(changed)+len(added) > 0 {
		t.Fatalf("filesystem change %v", base(removed))
	}
	for _, kept := range []string{a2h, aNew, bNew, bHeld} {
		if !exists(kept) {
			t.Fatalf("deleted %s", kept)
		}
	}
	if !strings.HasPrefix(real.StopLine, "prune stop: age rule reached its cutoff ") || !strings.Contains(real.StopLine, " after 4 deletion(s), stopping at "+filepath.Base(a2h)) {
		t.Fatalf("stop line %q", real.StopLine)
	}
	if r.log.count(msgLike("kept: not older than its newest segment")) == 0 {
		t.Fatal("the held segment was not logged")
	}
}

func TestPruneWatermark(t *testing.T) {
	r := newRig(t)
	now := r.now
	files := map[string]string{}
	for _, c := range []struct {
		name  string
		hours []int
	}{{"Cam_A", []int{40, 30, 20}}, {"Cam_B", []int{38, 28, 18}}, {"Cam_C", []int{36, 26, 16}}} {
		for _, h := range c.hours {
			files[c.name+"-"+strconv.Itoa(h)] = r.seg(c.name, now.Add(-time.Duration(h)*time.Hour), unit)
		}
		files[c.name+"-new"] = r.seg(c.name, now.Add(-time.Minute), unit)
	}
	r.claim()
	r.fs.Reset(10)

	r.rules.ReserveGB = 5
	idle := r.run(false)
	if len(idle.Deletions) != 0 || !strings.Contains(idle.StopLine, "watermark not needed") {
		t.Fatalf("above the reserve: %d deleted, %q", len(idle.Deletions), idle.StopLine)
	}

	r.rules.ReserveGB, r.rules.PruneHysteresisGB = 13, 2
	first := r.run(false)
	want := []string{files["Cam_A-40"], files["Cam_B-38"], files["Cam_C-36"], files["Cam_A-30"], files["Cam_B-28"]}
	if !eqList(paths(first.Deletions), want) || first.Blocked {
		t.Fatalf("deleted %v blocked %v", base(paths(first.Deletions)), first.Blocked)
	}
	if !strings.Contains(first.StopLine, "watermark reached its target 15.00 GB after 5 deletion(s)") || !strings.HasSuffix(first.StopLine, "stopping at "+filepath.Base(files["Cam_C-26"])) {
		t.Fatalf("stop line %q", first.StopLine)
	}

	r.rules.ReserveGB = 1000
	blocked, again := r.run(false), r.run(false)
	if len(blocked.Deletions) != 4 || !blocked.Blocked || !strings.HasPrefix(blocked.Message, "PRUNE BLOCKED") || len(again.Deletions) != 0 || !again.Blocked {
		t.Fatalf("reserve 1000: deleted %d blocked %v (%q); again %d %v", len(blocked.Deletions), blocked.Blocked, blocked.Message, len(again.Deletions), again.Blocked)
	}
	if !strings.Contains(blocked.StopLine, "watermark: nothing deletable left") {
		t.Fatalf("stop line %q", blocked.StopLine)
	}
	if n := r.log.count(func(x rec) bool { return strings.HasPrefix(x.Msg, "PRUNE BLOCKED") && x.Level.String() == "ERROR" }); n < 2 {
		t.Fatalf("BLOCKED logged %d times", n)
	}
	for _, c := range []string{"Cam_A", "Cam_B", "Cam_C"} {
		if !exists(files[c+"-new"]) {
			t.Fatalf("deleted the newest segment of %s", c)
		}
	}
}

func TestPruneClock(t *testing.T) {
	plain := func(r *rig) {
		r.seg("Front_Entrance", r.now.Add(-3*day))
		r.seg("Front_Entrance", r.now.Add(-2*day))
		r.seg("Front_Entrance", r.now.Add(-time.Minute))
	}
	future := func(r *rig) {
		r.seg("Front_Entrance", r.now.Add(-3*day))
		late := r.seg("Front_Entrance", r.now.Add(-2*day))
		r.touch(late, r.now.Add(2*day))
		r.seg("Front_Entrance", r.now.Add(-time.Minute))
	}
	inverted := func(r *rig) {
		// The newest by name - the protected one - was written ten days ago; the only other
		// segment was written an hour ago.
		c := r.seg("Front_Entrance", r.now.Add(-day))
		r.touch(c, r.now.Add(-time.Hour))
		p := r.seg("Front_Entrance", r.now.Add(-time.Minute))
		r.touch(p, r.now.Add(-10*day))
	}
	for _, c := range []struct {
		name  string
		build func(*rig)
		shift time.Duration
		want  string
	}{
		{"ahead", plain, 30 * day, "ahead of the newest segment"},
		{"future", future, 0, "in the future of the wall clock"},
		{"inverted", inverted, 0, "is later than the newest segment"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			r.rules.MaxRetentionDays, r.rules.MaxAgePruneFraction = ptr(1), 1.0
			c.build(r)
			r.claim()
			before := r.manifest()
			r.now = r.now.Add(c.shift)
			first, second := r.run(false), r.run(false)
			removed, changed, added := diffManifest(before, r.manifest())
			logged := r.log.count(func(x rec) bool { return strings.HasPrefix(x.Msg, "PRUNE REFUSED") && x.Level.String() == "WARN" })
			if !first.Blocked || first.Refusal == nil || first.Refusal.Gate != GateClock || !strings.Contains(first.Refusal.Reason, c.want) ||
				len(first.Deletions)+len(second.Deletions) > 0 || len(removed)+len(changed)+len(added) > 0 || logged != 1 || len(first.Alerts)+len(second.Alerts) > 0 {
				t.Fatalf("refusal %+v, deleted %d, changed %d, logged %d", first.Refusal, len(first.Deletions)+len(second.Deletions), len(removed)+len(changed)+len(added), logged)
			}
		})
	}
	t.Run("control", func(t *testing.T) {
		r := newRig(t)
		r.rules.MaxRetentionDays, r.rules.MaxAgePruneFraction = ptr(1), 1.0
		plain(r)
		r.claim()
		if res := r.run(false); res.Refusal != nil || len(res.Deletions) != 2 {
			t.Fatalf("control: %+v, deleted %d", res.Refusal, len(res.Deletions))
		}
	})
}

// breakerRig: ten segments over two cameras, expired of them five days old, the rest recent.
func breakerRig(t *testing.T, expired, expiredBytes, recentBytes int) (*rig, []string) {
	r := newRig(t)
	r.rules.MaxRetentionDays, r.rules.MaxAgePruneFraction = ptr(1), 0.5
	cams := []string{"Cam_A", "Cam_B"}
	var old []string
	for i := 0; i < expired; i++ {
		old = append(old, r.seg(cams[i%2], r.now.Add(-5*day+time.Duration(i)*time.Hour), expiredBytes))
	}
	for i := 0; i < 8-expired; i++ {
		r.seg(cams[i%2], r.now.Add(-time.Duration(2+i)*time.Hour), recentBytes)
	}
	for _, c := range cams {
		r.seg(c, r.now.Add(-time.Minute), recentBytes)
	}
	r.claim()
	r.fs.Reset(10)
	return r, old
}

func TestPruneBreaker(t *testing.T) {
	held := func(r *rig) int {
		return r.log.count(func(x rec) bool {
			return strings.HasPrefix(x.Msg, "PRUNE AGE RULE HELD") && x.Level.String() == "ERROR"
		})
	}
	alerts := func(rs ...Result) (ids []int) {
		for _, x := range rs {
			for _, a := range x.Alerts {
				ids = append(ids, a.ID)
			}
		}
		return
	}

	r, _ := breakerRig(t, 6, 1024, 1024)
	before := r.manifest()
	first, second := r.run(false), r.run(false)
	removed, changed, added := diffManifest(before, r.manifest())
	if len(first.Deletions)+len(second.Deletions) > 0 || len(removed)+len(changed)+len(added) > 0 {
		t.Fatal("the age rule deleted past the breaker")
	}
	if held(r) != 1 || !strings.Contains(first.Breaker, "6 of 10 segments (60.0%)") || !strings.Contains(first.Breaker, "(60.0%) of the bytes") || !strings.Contains(first.Breaker, "MaxAgePruneFraction 0.5") {
		t.Fatalf("held lines %d, breach %q", held(r), first.Breaker)
	}
	if ids := alerts(first, second); len(ids) != 1 || ids[0] != AlertBreaker || first.Alerts[0].Level != "Error" {
		t.Fatalf("alerts %v", ids)
	}
	if !strings.HasPrefix(first.StopLine, "prune stop: age rule HELD by the circuit breaker") || !strings.Contains(first.StopLine, "; watermark not needed") {
		t.Fatalf("stop line %q", first.StopLine)
	}
	r.rules.MaxRetentionDays = ptr(30)
	calm := r.run(false)
	r.rules.MaxRetentionDays = ptr(1)
	again := r.run(false)
	if len(calm.Deletions)+len(again.Deletions) > 0 || len(alerts(first, second, calm, again)) != 2 || held(r) != 2 {
		t.Fatalf("a new episode did not alert again: %v, held %d", alerts(first, second, calm, again), held(r))
	}

	r, old := breakerRig(t, 4, 1024, 1024)
	res := r.run(false)
	if !eqList(paths(res.Deletions), old) || held(r) != 0 || len(res.Alerts) != 0 {
		t.Fatalf("40%%: deleted %v", base(paths(res.Deletions)))
	}

	for _, c := range []struct {
		name                 string
		expired, eb, rb      int
		count, bytes, needle string
	}{
		{"bytes", 4, 4096, 1024, "4 of 10 segments (40.0%)", "(72.7%) of the bytes", ""},
		{"count", 6, 512, 8192, "6 of 10 segments (60.0%)", "(8.6%) of the bytes", ""},
	} {
		r, _ := breakerRig(t, c.expired, c.eb, c.rb)
		res := r.run(false)
		if len(res.Deletions) != 0 || held(r) != 1 || !strings.Contains(res.Breaker, c.count) || !strings.Contains(res.Breaker, c.bytes) || len(res.Alerts) != 1 {
			t.Fatalf("breach by %s: deleted %d, %q", c.name, len(res.Deletions), res.Breaker)
		}
	}

	r, old = breakerRig(t, 6, unit, unit)
	r.rules.ReserveGB, r.rules.PruneHysteresisGB = 13, 2
	res = r.run(false)
	byAge := 0
	for _, d := range res.Deletions {
		if d.Reason == ReasonAge {
			byAge++
		}
	}
	if !res.BreakerHeld || !eqList(paths(res.Deletions), old[:5]) || byAge != 0 || res.Blocked || !strings.Contains(res.StopLine, "watermark reached its target 15.00 GB after 5 deletion(s)") {
		t.Fatalf("held %v, deleted %v, by age %d, %q", res.BreakerHeld, base(paths(res.Deletions)), byAge, res.StopLine)
	}
}

func TestPruneBlocked(t *testing.T) {
	r := newRig(t)
	r.rules.ReserveGB = 20
	old := r.seg("Front_Entrance", r.now.Add(-3*time.Hour))
	newest := r.seg("Front_Entrance", r.now.Add(-time.Minute))
	r.claim()
	r.fs.Reset(1)
	r.fs.Fixed = true

	dry := r.run(true)
	if !dry.Blocked || len(dry.Alerts) != 0 || !strings.HasPrefix(dry.Message, "would be BLOCKED") {
		t.Fatalf("dry run: blocked %v, alerts %d, %q", dry.Blocked, len(dry.Alerts), dry.Message)
	}
	var runs []Result
	var after []int
	total := 0
	step := func() {
		res := r.run(false)
		runs = append(runs, res)
		total += len(res.Alerts)
		after = append(after, total)
	}
	step()
	step()
	r.rules.ReserveGB = 0
	step()
	r.rules.ReserveGB = 20
	step()
	if !runs[0].Blocked || !runs[1].Blocked || runs[2].Blocked || !runs[3].Blocked {
		t.Fatal("the runs were not blocked, blocked, clear, blocked")
	}
	if !eqInts(after, []int{1, 1, 1, 2}) {
		t.Fatalf("alerts after each run %v, want 1 1 1 2", after)
	}
	if len(runs[0].Deletions) != 1 || exists(old) || !exists(newest) {
		t.Fatal("the blocked run did not delete the one deletable segment and keep the newest")
	}
	for _, x := range runs {
		for _, a := range x.Alerts {
			for _, want := range []string{"free 1.00 GB", "20.00 GB reserve", "ReserveGB", "RootPath", "free space", "Recording continues"} {
				if a.ID != AlertBlocked || a.Level != "Error" || !strings.Contains(a.Body, want) {
					t.Fatalf("alert %+v lacks %q", a, want)
				}
			}
		}
	}
}

func eqInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPruneConfig(t *testing.T) {
	p := osx.Current()
	for _, bad := range []string{"0", "-1", "1.5", `"abc"`} {
		_, _, err := config.Parse([]byte(`{ "MaxAgePruneFraction": `+bad+` }`), p)
		if err == nil || !strings.Contains(err.Error(), "MaxAgePruneFraction") {
			t.Fatalf("%s: %v", bad, err)
		}
		// A non-number is refused as "must be a number" without the range (internal/config wording,
		// listed in testdata/golden/prune/EXPECTED-DIFFERENCES.md); out-of-range values name it.
		if bad != `"abc"` && (!strings.Contains(err.Error(), "0.01") || !strings.Contains(err.Error(), "1.0")) {
			t.Fatalf("%s: %v", bad, err)
		}
	}
	for text, want := range map[string]float64{"1.0": 1.0, "0.5": 0.5} {
		c, _, err := config.Parse([]byte(`{ "MaxAgePruneFraction": `+text+` }`), p)
		if err != nil || c.MaxAgePruneFraction != want || RulesFrom(c).MaxAgePruneFraction != want {
			t.Fatalf("%s: %v %v", text, c.MaxAgePruneFraction, err)
		}
	}
	c, _, err := config.Parse([]byte(`{ "ReserveGB": 20 }`), p)
	if err != nil || c.MaxAgePruneFraction != 0.5 {
		t.Fatalf("default %v %v", c.MaxAgePruneFraction, err)
	}
	if r := RulesFrom(c); r.Container != ".mkv" || r.ReserveGB != 20 || r.PruneHysteresisGB != 5 || r.MaxRetentionDays != nil || r.SegmentSeconds != 900 || r.StallFactor != 2.5 {
		t.Fatalf("rules %+v", r)
	}
}

func TestPruneOwner(t *testing.T) {
	t.Run("clean", func(t *testing.T) {
		r := newRig(t)
		r.rules.MaxRetentionDays = ptr(1)
		r.seg("pruneowner-clean", r.now.Add(-time.Minute))
		res, err := archive.ClaimRoot(r.root, osx.Current(), ".mkv", r.now)
		if err != nil || !res.Created {
			t.Fatalf("a clean root was not claimed: %+v %v", res, err)
		}
		if x := r.run(false); x.Refusal != nil || len(x.Alerts) != 0 {
			t.Fatalf("clean root: %+v %v", x.Refusal, x.Alerts)
		}
	})
	t.Run("foreign", func(t *testing.T) {
		r := newRig(t)
		r.rules.MaxRetentionDays = ptr(1)
		r.put(filepath.Join(r.root, "Photos", "holiday.jpg"), 3, r.now)
		bait := []string{
			r.seg("pruneowner-foreign", r.now.Add(-5*day)), r.seg("pruneowner-foreign", r.now.Add(-4*day)),
			r.seg("pruneowner-foreign", r.now.Add(-time.Minute)),
		}
		if res, _ := archive.ClaimRoot(r.root, osx.Current(), ".mkv", r.now); res.Created || exists(archive.MarkerPath(r.root)) {
			t.Fatal("a root with a foreign entry was claimed")
		}
		before := r.manifest()
		var alerts []Alert
		for i := 0; i < 3; i++ {
			alerts = append(alerts, r.run(false).Alerts...)
		}
		removed, changed, added := diffManifest(before, r.manifest())
		if len(alerts) != 1 || alerts[0].ID != AlertDisabled || alerts[0].Level != "Error" || alerts[0].Title != "pruning is disabled - the disk will fill" ||
			len(removed)+len(changed)+len(added) > 0 || !r.st.Blocked || !strings.Contains(r.st.LastMessage, "Photos") {
			t.Fatalf("alerts %+v, files changed %d, state %+v", alerts, len(removed)+len(changed)+len(added), r.st)
		}
		for _, b := range bait {
			if !exists(b) {
				t.Fatal("bait deleted")
			}
		}
	})
}

func TestPruneLoop(t *testing.T) {
	// The supervisor loop's prune phases as consecutive runs over one archive: a camera that is
	// recording (6 s segments, the newest written 2 s ago) beside planted fixtures.
	newLoop := func(t *testing.T, extra func(*rig)) (*rig, []string) {
		r := newRig(t)
		r.rules.SegmentSeconds = 6
		var rec []string
		for i := 4; i >= 0; i-- {
			p := r.seg("synth1", r.now.Add(-time.Duration(i*6+8)*time.Second))
			r.touch(p, r.now.Add(-time.Duration(i*6+2)*time.Second))
			rec = append(rec, p)
		}
		r.claim()
		extra(r)
		return r, rec
	}
	t.Run("age", func(t *testing.T) {
		var expired, kept []string
		r, rec := newLoop(t, func(r *rig) {
			expired = []string{r.seg("synth1", r.now.Add(-3*day)), r.seg("synth1", r.now.Add(-2*day)), r.seg("synth1", r.now.Add(-30*time.Hour)),
				r.seg("oldcam", r.now.Add(-4*day)), r.seg("oldcam", r.now.Add(-84*time.Hour))}
			kept = []string{r.seg("synth1", r.now.Add(-12*time.Hour)), r.seg("oldcam", r.now.Add(-3*day)),
				r.segExt("synth1", "synth1", r.now.Add(-5*day), ".mp4"), r.put(filepath.Join(r.root, "synth1", "notes.txt"), 13, r.now.Add(-6*day))}
		})
		r.rules.MaxRetentionDays, r.rules.MaxAgePruneFraction = ptr(1), 1.0
		var deleted []string
		for i := 0; i < 3; i++ {
			deleted = append(deleted, paths(r.run(false).Deletions)...)
		}
		if !eqList(sorted(deleted), sorted(expired)) {
			t.Fatalf("deleted %v, want %v", base(deleted), base(expired))
		}
		for _, k := range append(kept, rec...) {
			if !exists(k) {
				t.Fatalf("deleted %s", k)
			}
		}
		if n := r.log.count(msgLike("prune stop: ")); n != 3 {
			t.Fatalf("%d stop lines over 3 runs", n)
		}
	})
	t.Run("breaker", func(t *testing.T) {
		var flood []string
		r, _ := newLoop(t, func(r *rig) {
			for i := 0; i < 8; i++ {
				flood = append(flood, r.seg("synth1", r.now.Add(-3*day+time.Duration(i)*time.Hour)))
			}
		})
		r.rules.MaxRetentionDays, r.rules.MaxAgePruneFraction = ptr(1), 0.5
		var ids []int
		runs := func(n int) {
			for i := 0; i < n; i++ {
				res := r.run(false)
				if len(res.Deletions) > 0 && !(res.Deletions[0].Reason == ReasonAge && !res.BreakerHeld) {
					t.Fatalf("deleted %v while held", base(paths(res.Deletions)))
				}
				for _, a := range res.Alerts {
					ids = append(ids, a.ID)
				}
			}
		}
		runs(3)
		if !eqInts(ids, []int{AlertBreaker}) {
			t.Fatalf("first flood alerts %v", ids)
		}
		for _, p := range flood[:6] { // thin the expired set below the brake: 2 of 7
			os.Remove(p)
		}
		runs(1) // deletes the two and resets the breaker
		for i := 0; i < 8; i++ {
			r.seg("synth1", r.now.Add(-50*time.Hour+time.Duration(i)*time.Hour))
		}
		runs(3)
		if !eqInts(ids, []int{AlertBreaker, AlertBreaker}) {
			t.Fatalf("alerts %v, want 122 twice", ids)
		}
	})
	t.Run("blocked", func(t *testing.T) {
		r, rec := newLoop(t, func(r *rig) {
			r.seg("oldcam", r.now.Add(-2*day))
			r.seg("oldcam", r.now.Add(-day))
		})
		r.fs.Reset(10)
		r.rules.ReserveGB = 1e6
		var ids []int
		for i := 0; i < 4; i++ {
			res := r.run(false)
			if !res.Blocked {
				t.Fatalf("run %d not blocked", i)
			}
			for _, a := range res.Alerts {
				ids = append(ids, a.ID)
			}
		}
		if !eqInts(ids, []int{AlertBlocked}) || !exists(rec[len(rec)-1]) {
			t.Fatalf("alerts %v, newest kept %v", ids, exists(rec[len(rec)-1]))
		}
	})
}

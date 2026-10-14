package status

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 13, 21, 0, 0, 0, time.UTC)

func TestJobLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.jsonl")
	if jobs, err := Recent(path, 5); jobs != nil || err != nil {
		t.Errorf("Recent on a missing log = %v, %v", jobs, err)
	}
	for i := range 3 {
		j := Job{Repo: "octo/site", Class: "boxrunner", Runner: "r" + string(rune('1'+i)), Result: "succeeded",
			Queued: t0, Started: t0.Add(8 * time.Second), Finished: t0.Add(time.Duration(i+1) * time.Minute)}
		if err := Append(path, j); err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := Recent(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[0].Runner != "r2" || jobs[1].Runner != "r3" {
		t.Errorf("Recent = %+v", jobs)
	}
	if jobs[1].Wait() != 8*time.Second || jobs[1].Took() != 3*time.Minute-8*time.Second {
		t.Errorf("wait %v, took %v", jobs[1].Wait(), jobs[1].Took())
	}
}

func TestJobLogTrims(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.jsonl")
	long := strings.Repeat("x", 3000)
	for range 400 {
		if err := Append(path, Job{Repo: "octo/site", Name: long, Finished: t0}); err != nil {
			t.Fatal(err)
		}
	}
	jobs, _ := Recent(path, 10_000)
	if len(jobs) >= 400 || len(jobs) < 150 || jobs[len(jobs)-1].Finished != t0 {
		t.Errorf("log holds %d jobs after trimming", len(jobs))
	}
}

func TestServeAndFetch(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "s.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	want := Snapshot{Box: "box-1", Version: "0.1.0", Runners: []Runner{{Name: "r1", Repo: "octo/site", Class: "boxrunner", Phase: "running", Since: t0}}}
	var drains []bool
	go Serve(ctx, socket, func() Snapshot { return want }, func(on bool) error {
		drains = append(drains, on)
		if len(drains) == 3 {
			return errors.New("writing /var/lib/boxrunner/draining: read-only file system")
		}
		return nil
	})
	var got Snapshot
	var err error
	for range 50 {
		if got, err = Fetch(ctx, socket); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("Fetch = %+v, %v", got, err)
	}
	if err := Drain(ctx, socket, true); err != nil {
		t.Errorf("Drain = %v", err)
	}
	if err := Drain(ctx, socket, false); err != nil {
		t.Errorf("Drain(false) = %v", err)
	}
	if err := Drain(ctx, socket, true); err == nil || err.Error() != "the agent refused /drain: writing /var/lib/boxrunner/draining: read-only file system" {
		t.Errorf("Drain that the agent refuses = %v", err)
	}
	if want := []bool{true, false, true}; !reflect.DeepEqual(drains, want) {
		t.Errorf("drain calls = %v, want %v", drains, want)
	}
}

func TestServeRemovesSocket(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "s.sock")
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- Serve(ctx, socket, func() Snapshot { return Snapshot{} }, func(bool) error { return nil })
	}()
	for range 50 {
		if _, err := Fetch(ctx, socket); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-served; err != nil {
		t.Errorf("Serve = %v", err)
	}
	if _, err := os.Stat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("after Serve returned, the socket is still there: %v", err)
	}
}

func TestDrained(t *testing.T) {
	running := Runner{Name: "r1", Repo: "octo/site", Class: "boxrunner", Phase: "running", Since: t0}
	kept := Runner{Name: "r2", Repo: "octo/site", Class: "boxrunner", Phase: "exited", Since: t0}
	cases := []struct {
		name string
		snap Snapshot
		want bool
	}{
		{"serving", Snapshot{}, false},
		{"a session still open", Snapshot{Draining: true, ScaleSets: []ScaleSet{{"octo/site", "boxrunner", 0}}}, false},
		{"a job still running", Snapshot{Draining: true, Runners: []Runner{running, kept}}, false},
		{"only a kept failed container", Snapshot{Draining: true, Runners: []Runner{kept}}, true},
		{"nothing left", Snapshot{Draining: true}, true},
	}
	for _, c := range cases {
		if got := c.snap.Drained(); got != c.want {
			t.Errorf("%s: Drained = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPrintDraining(t *testing.T) {
	snap := Snapshot{Box: "box-1", Version: "0.1.0", DiskFree: 41_250_000_000, DiskFloor: 10_000_000_000, Draining: true}
	var b strings.Builder
	Print(&b, snap, t0)
	want := "box-1  boxrunner 0.1.0  worker unknown  image none (latest none)  disk 41.2 GB free, floor 10.0 GB\n\n" +
		"Draining: the agent closed its sessions and starts no runners; running jobs finish. Resume with: sudo boxrunner resume\n\n" +
		"CLASS  PRIORITY  RUNNERS  MAX\n"
	if got := b.String(); !strings.HasPrefix(got, want) {
		t.Errorf("Print =\n%s\nwant it to start with\n%s", got, want)
	}
}

func TestPrint(t *testing.T) {
	snap := Snapshot{
		Box: "box-1", Version: "0.1.0", Worker: "0.1.0", Latest: "0.1.0", Image: ImageStatus{Built: "2.338.0", Latest: "2.338.0", Disk: 3_140_000_000},
		DiskFree: 41_250_000_000, DiskFloor: 10_000_000_000,
		Classes:   []Class{{Name: "boxrunner", Priority: "normal", Max: 2, Active: 2}, {Name: "boxrunner-light", Priority: "idle", Max: 2}},
		ScaleSets: []ScaleSet{{"octo/site", "boxrunner", 1}},
		Elsewhere: []Elsewhere{{"octo/docs", "boxrunner", "box-2"}},
		Runners: []Runner{
			{Name: "box-1-boxrunner-3f9a2c", Repo: "octo/site", Class: "boxrunner", Phase: "running", Since: t0},
			{Name: "box-1-boxrunner-8e41b7", Repo: "octo/site", Class: "boxrunner", Phase: "starting", Since: t0.Add(time.Minute), Warm: true},
		},
		Jobs: []Job{{Repo: "octo/site", Class: "boxrunner", Name: "test", Result: "succeeded",
			Queued: t0.Add(-time.Hour), Started: t0.Add(-time.Hour + 8*time.Second), Finished: t0.Add(-50 * time.Minute)}},
		Started: t0, FreshStart: true,
	}
	var b strings.Builder
	Print(&b, snap, t0.Add(4*time.Minute+10*time.Second))
	want := `box-1  boxrunner 0.1.0  worker 0.1.0  image 2.338.0 (latest 2.338.0)  images 3.1 GB  disk 41.2 GB free, floor 10.0 GB

If this box replaced one that died, jobs the old box had acquired stay queued: see docs/architecture.md#replacing-a-box.

CLASS            PRIORITY  RUNNERS  MAX
boxrunner        normal    2        2
boxrunner-light  idle      0        2

REPOSITORY  CLASS      ASSIGNED JOBS
octo/site   boxrunner  1

box-2 serves octo/docs boxrunner; this box takes its jobs only after box-2 stops. Give each box its own classes or repos (docs/config.md#several-boxes).

RUNNER                  REPOSITORY  CLASS      STATE            FOR
box-1-boxrunner-3f9a2c  octo/site   boxrunner  running          4m10s
box-1-boxrunner-8e41b7  octo/site   boxrunner  starting (warm)  3m10s

FINISHED      REPOSITORY  CLASS      JOB   RESULT     QUEUED FOR  TOOK
` + t0.Add(-50*time.Minute).Local().Format("Jan 02 15:04") + `  octo/site   boxrunner  test  succeeded  8s          9m52s
`
	if got := b.String(); got != want {
		t.Errorf("Print =\n%s\nwant\n%s", got, want)
	}

	b.Reset()
	Print(&b, snap, t0.Add(30*time.Minute))
	if strings.Contains(b.String(), "replaced") {
		t.Errorf("30 minutes after the start, Print still points to replacing a box:\n%s", b.String())
	}
}

func TestPrintHeldClass(t *testing.T) {
	snap := Snapshot{
		Box: "box-1", Version: "0.1.0", DiskFree: 41_250_000_000, DiskFloor: 10_000_000_000,
		Classes: []Class{{Name: "boxrunner", Priority: "normal", Max: 1}, {Name: "web", Priority: "normal", Max: 1, HeldUntil: t0.Add(4 * time.Minute)}},
	}
	var b strings.Builder
	Print(&b, snap, t0)
	want := "box-1  boxrunner 0.1.0  worker unknown  image none (latest none)  disk 41.2 GB free, floor 10.0 GB\n\n" +
		"Runners of class web exit before taking a job; next try at " + t0.Add(4*time.Minute).Local().Format("15:04:05") + ". See `journalctl -u boxrunner` for their output.\n\n" +
		"CLASS      PRIORITY  RUNNERS  MAX\n"
	if got := b.String(); !strings.HasPrefix(got, want) {
		t.Errorf("Print =\n%s\nwant it to start with\n%s", got, want)
	}
	if got, want := snap.Classes[1].NextTry(t0.Add(5*time.Minute)), "trying one runner now"; got != want {
		t.Errorf("NextTry after the hold = %q, want %q", got, want)
	}
	if got := snap.Classes[0].NextTry(t0); got != "" {
		t.Errorf("NextTry of a class that works = %q", got)
	}
}

func TestPrintSaysWhenANewerReleaseIsOut(t *testing.T) {
	header := func(s Snapshot) string {
		var b strings.Builder
		Print(&b, s, t0)
		head, _, _ := strings.Cut(b.String(), "CLASS")
		return head
	}
	snap := Snapshot{Box: "box-1", Version: "0.2.1", Worker: "0.2.0", Latest: "0.2.2", DiskFloor: 10e9}
	want := `box-1  boxrunner 0.2.1  worker 0.2.0  image none (latest none)  disk 0.0 GB free, floor 10.0 GB

boxrunner 0.2.2 is out. Upgrade this agent with: sudo boxrunner upgrade

The Worker runs 0.2.0. Redeploy it from 0.2.2: see docs/upgrading.md.

`
	if got := header(snap); got != want {
		t.Errorf("behind:\n%s\nwant\n%s", got, want)
	}

	snap.Version, snap.Worker = "0.2.2", ""
	want = `box-1  boxrunner 0.2.2  worker unknown  image none (latest none)  disk 0.0 GB free, floor 10.0 GB

`
	if got := header(snap); got != want {
		t.Errorf("current, with a Worker that names no version:\n%s\nwant\n%s", got, want)
	}
}

func TestCount(t *testing.T) {
	site := Job{Repo: "octo/site", Class: "boxrunner", Result: "succeeded", Queued: t0, Started: t0.Add(8 * time.Second), Finished: t0.Add(68 * time.Second)}
	slow := Job{Repo: "octo/site", Class: "boxrunner", Result: "succeeded", Queued: t0, Started: t0.Add(5 * time.Second), Finished: t0.Add(7 * time.Hour)}
	// A job logged before boxrunner knew when it queued.
	docs := Job{Repo: "octo/docs", Class: "boxrunner", Result: "failed", Started: t0, Finished: t0.Add(30 * time.Second)}
	first := Count(nil, site)
	got := Count(Count(first, slow), docs)
	want := []Tally{
		{Repo: "octo/docs", Class: "boxrunner", Result: "failed", Jobs: 1,
			Took: Histogram{Counts: [14]uint64{3: 1}, Sum: 30}},
		{Repo: "octo/site", Class: "boxrunner", Result: "succeeded", Jobs: 2,
			Took: Histogram{Counts: [14]uint64{4: 1, 13: 1}, Sum: 60 + 25195},
			Wait: Histogram{Counts: [14]uint64{1: 1, 2: 1}, Sum: 13}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Count =\n%+v\nwant\n%+v", got, want)
	}
	if want := []Tally{{Repo: "octo/site", Class: "boxrunner", Result: "succeeded", Jobs: 1,
		Took: Histogram{Counts: [14]uint64{4: 1}, Sum: 60}, Wait: Histogram{Counts: [14]uint64{2: 1}, Sum: 8}}}; !reflect.DeepEqual(first, want) {
		t.Errorf("Count changed the tallies it was given: %+v", first)
	}
}

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

	"github.com/dominikzabcik/boxrunner/internal/autoupgrade"
	"github.com/dominikzabcik/boxrunner/internal/egress"
	"github.com/dominikzabcik/boxrunner/internal/seen"
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
		{"a session still open", Snapshot{Draining: true, ScaleSets: []ScaleSet{{Repo: "octo/site", Class: "boxrunner"}}}, false},
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
	want := "box-1  boxrunner 0.1.0  worker unknown  image none (latest none)  disk 41.2 GB free, floor 10.0 GB  auto-upgrade off\n\n" +
		"Draining: the agent closed its sessions and starts no runners; running jobs finish. Resume with: sudo boxrunner resume\n\n" +
		"CLASS  PRIORITY  RUNNERS  MAX  DASHBOARD\n"
	if got := b.String(); !strings.HasPrefix(got, want) {
		t.Errorf("Print =\n%s\nwant it to start with\n%s", got, want)
	}
}

func TestPrintJSON(t *testing.T) {
	snap := Snapshot{
		Box: "box-1", Version: "0.1.0", Worker: "0.1.0",
		Classes: []Class{{Name: "boxrunner", Priority: "normal", Max: 2, Active: 1}, {Name: "boxrunner-light", Priority: "idle", Max: 2, Limit: new(0), Paused: true}},
		Runners: []Runner{{Name: "box-1-boxrunner-3f9a2c", Repo: "octo/site", Class: "boxrunner", Phase: "running", Since: t0}},
		Image:   ImageStatus{Built: "2.338.0", Latest: "2.338.0"}, DiskFree: 41_250_000_000, DiskFloor: 10_000_000_000,
	}
	var b strings.Builder
	if err := PrintJSON(&b, snap); err != nil {
		t.Fatal(err)
	}
	want := `{
  "box": "box-1",
  "version": "0.1.0",
  "classes": [
    {
      "name": "boxrunner",
      "priority": "normal",
      "max": 2,
      "active": 1,
      "paused": false
    },
    {
      "name": "boxrunner-light",
      "priority": "idle",
      "max": 2,
      "active": 0,
      "limit": 0,
      "paused": true
    }
  ],
  "scaleSets": null,
  "runners": [
    {
      "name": "box-1-boxrunner-3f9a2c",
      "repo": "octo/site",
      "class": "boxrunner",
      "phase": "running",
      "since": "2026-10-13T21:00:00Z"
    }
  ],
  "image": {
    "built": "2.338.0",
    "latest": "2.338.0"
  },
  "diskFree": 41250000000,
  "diskFloor": 10000000000,
  "worker": "0.1.0"
}
`
	if got := b.String(); got != want {
		t.Errorf("PrintJSON =\n%s\nwant\n%s", got, want)
	}
}

func TestPrint(t *testing.T) {
	snap := Snapshot{
		Box: "box-1", Version: "0.1.0", Worker: "0.1.0", Latest: "0.1.0", Image: ImageStatus{Built: "2.338.0", Latest: "2.338.0", Disk: 3_140_000_000},
		DiskFree: 41_250_000_000, DiskFloor: 10_000_000_000,
		Classes: []Class{{Name: "boxrunner", Priority: "normal", Max: 2, Active: 2}, {Name: "boxrunner-light", Priority: "idle", Max: 2, Limit: new(0)},
			{Name: "boxrunner-gpu", Priority: "high", Max: 1, Paused: true}, {Name: "boxrunner-arm", Priority: "normal", Max: 4, Limit: new(1), Paused: true}},
		ScaleSets: []ScaleSet{{Repo: "octo/site", Class: "boxrunner", Assigned: 1}},
		Elsewhere: []Elsewhere{{Repo: "octo/docs", Class: "boxrunner", Box: "box-2"}},
		Runners: []Runner{
			{Name: "box-1-boxrunner-3f9a2c", Repo: "octo/site", Class: "boxrunner", Phase: "running", Since: t0},
			{Name: "box-1-boxrunner-8e41b7", Repo: "octo/site", Class: "boxrunner", Phase: "starting", Since: t0.Add(time.Minute), Warm: true},
		},
		Jobs: []Job{
			{Repo: "octo/site", Class: "boxrunner", Name: "test", Result: "succeeded",
				Queued: t0.Add(-time.Hour), Started: t0.Add(-time.Hour + 8*time.Second), Finished: t0.Add(-50 * time.Minute),
				Usage: &Usage{CPUSeconds: 754.25, MemoryPeak: 9812000768}},
			{Repo: "octo/site", Class: "boxrunner", Name: "lint", Result: "failed",
				Queued: t0.Add(-2 * time.Hour), Started: t0.Add(-2 * time.Hour), Finished: t0.Add(-110 * time.Minute)},
		},
		Started: t0, FreshStart: true,
	}
	var b strings.Builder
	Print(&b, snap, t0.Add(4*time.Minute+10*time.Second))
	want := `box-1  boxrunner 0.1.0  worker 0.1.0  image 2.338.0 (latest 2.338.0)  images 3.1 GB  disk 41.2 GB free, floor 10.0 GB  auto-upgrade off

If this box replaced one that died, jobs the old box had acquired stay queued: see docs/architecture.md#replacing-a-box.

CLASS            PRIORITY  RUNNERS  MAX  DASHBOARD
boxrunner        normal    2        2    -
boxrunner-light  idle      0        2    limit 0, off on this box
boxrunner-gpu    high      0        1    paused
boxrunner-arm    normal    0        4    paused, limit 1

REPOSITORY  CLASS      ASSIGNED JOBS
octo/site   boxrunner  1

box-2 serves octo/docs boxrunner; this box takes its jobs only after box-2 stops. A Worker from v0.9.0 shares them between the boxes (docs/config.md#one-class-on-several-boxes).

RUNNER                  REPOSITORY  CLASS      STATE            FOR
box-1-boxrunner-3f9a2c  octo/site   boxrunner  running          4m10s
box-1-boxrunner-8e41b7  octo/site   boxrunner  starting (warm)  3m10s

FINISHED      REPOSITORY  CLASS      JOB   RESULT     QUEUED FOR  TOOK   CPU     PEAK MEMORY
` + t0.Add(-50*time.Minute).Local().Format("Jan 02 15:04") + `  octo/site   boxrunner  test  succeeded  8s          9m52s  12m34s  9.8 GB
` + t0.Add(-110*time.Minute).Local().Format("Jan 02 15:04") + `  octo/site   boxrunner  lint  failed     0s          10m0s  ?       ?
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

func TestPrintRunnerImage(t *testing.T) {
	snap := Snapshot{
		Box: "box-1", Version: "0.1.0", DiskFloor: 10e9,
		Image: ImageStatus{
			Built: "2.337.0+3f9a2c0b1d4e", Digest: "sha256:e5496277be5d09bc968b3d64911b74e219ac4a3f2edce956a3ecf9271bea1ef4",
			Latest: "2.338.0+3f9a2c0b1d4e", Unlisted: "2.339.0",
			Refused: "ghcr.io/actions/actions-runner:2.338.0 points to sha256:0bad, not to sha256:4ffa that boxrunner's CI verified",
		},
	}
	var b strings.Builder
	Print(&b, snap, t0)
	want := `box-1  boxrunner 0.1.0  worker unknown  image 2.337.0+3f9a2c0b1d4e (latest 2.338.0+3f9a2c0b1d4e)  disk 0.0 GB free, floor 10.0 GB  auto-upgrade off

The job image is built from runner image sha256:e5496277be5d09bc968b3d64911b74e219ac4a3f2edce956a3ecf9271bea1ef4, which boxrunner's CI verified.

Runner 2.339.0 is out, but boxrunner's CI has not verified its image yet, so the agent builds 2.338.0 until it does.

The agent refused to build the job image, and jobs keep the current image: ghcr.io/actions/actions-runner:2.338.0 points to sha256:0bad, not to sha256:4ffa that boxrunner's CI verified

CLASS`
	if got := b.String(); !strings.HasPrefix(got, want) {
		t.Errorf("Print =\n%s\nwant it to start with\n%s", got, want)
	}
}

func TestPrintBuildingImage(t *testing.T) {
	cases := []struct {
		name string
		img  ImageStatus
		want string
	}{
		{"the first image", ImageStatus{Building: "2.338.0+e3cd41ebfb31"}, "image building 2.338.0+e3cd41ebfb31 (latest none)"},
		{"class images after a restart", ImageStatus{Building: "2.338.0+e3cd41ebfb31", Latest: "2.338.0+e3cd41ebfb31"}, "image building 2.338.0+e3cd41ebfb31 (latest 2.338.0+e3cd41ebfb31)"},
		{
			"a newer image", ImageStatus{Built: "2.337.0+e3cd41ebfb31", Building: "2.338.0+e3cd41ebfb31", Latest: "2.338.0+e3cd41ebfb31"},
			"image 2.337.0+e3cd41ebfb31, building 2.338.0+e3cd41ebfb31 (latest 2.338.0+e3cd41ebfb31)",
		},
	}
	for _, c := range cases {
		var b strings.Builder
		Print(&b, Snapshot{Box: "box-1", Version: "0.7.0", DiskFloor: 10e9, Image: c.img}, t0)
		want := "box-1  boxrunner 0.7.0  worker unknown  " + c.want + "  disk 0.0 GB free, floor 10.0 GB  auto-upgrade off\n"
		if got, _, _ := strings.Cut(b.String(), "\n"); got+"\n" != want {
			t.Errorf("%s: Print starts with\n%s\nwant\n%s", c.name, got, want)
		}
	}
}

func TestPrintHeldClass(t *testing.T) {
	snap := Snapshot{
		Box: "box-1", Version: "0.1.0", DiskFree: 41_250_000_000, DiskFloor: 10_000_000_000,
		Classes: []Class{{Name: "boxrunner", Priority: "normal", Max: 1}, {Name: "web", Priority: "normal", Max: 1, HeldUntil: t0.Add(4 * time.Minute)}},
	}
	var b strings.Builder
	Print(&b, snap, t0)
	want := "box-1  boxrunner 0.1.0  worker unknown  image none (latest none)  disk 41.2 GB free, floor 10.0 GB  auto-upgrade off\n\n" +
		"Runners of class web exit before taking a job; next try at " + t0.Add(4*time.Minute).Local().Format("15:04:05") + ". See `journalctl -u boxrunner` for their output.\n\n" +
		"CLASS      PRIORITY  RUNNERS  MAX  DASHBOARD\n"
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
	want := `box-1  boxrunner 0.2.1  worker 0.2.0  image none (latest none)  disk 0.0 GB free, floor 10.0 GB  auto-upgrade off

boxrunner 0.2.2 is out. Auto-upgrade is off. Upgrade this agent with: sudo boxrunner upgrade

The Worker runs 0.2.0. Redeploy it from 0.2.2: see docs/upgrading.md.

`
	if got := header(snap); got != want {
		t.Errorf("behind:\n%s\nwant\n%s", got, want)
	}

	snap.Version, snap.Worker = "0.2.2", ""
	want = `box-1  boxrunner 0.2.2  worker unknown  image none (latest none)  disk 0.0 GB free, floor 10.0 GB  auto-upgrade off

`
	if got := header(snap); got != want {
		t.Errorf("current, with a Worker that names no version:\n%s\nwant\n%s", got, want)
	}
}

func TestPrintAutoUpgrade(t *testing.T) {
	until := t0.Add(2 * time.Hour)
	at := until.Local().Format("Jan 02 15:04")
	cases := []struct {
		name string
		auto AutoUpgrade
		want string
	}{
		{"nothing decided yet", AutoUpgrade{Enabled: true}, "Auto-upgrade is on."},
		{"held for the delay", AutoUpgrade{Enabled: true, Verdict: autoupgrade.Hold, Until: until}, "Auto-upgrade installs it after " + at + "."},
		{"after a failed attempt", AutoUpgrade{Enabled: true, Verdict: autoupgrade.Retry, Until: until}, "Auto-upgrade tried a while ago and tries again after " + at + ". To upgrade now: sudo boxrunner upgrade"},
		{"draining", AutoUpgrade{Enabled: true, Verdict: autoupgrade.Draining}, "Auto-upgrade waits for the box to resume."},
		{"an egress job runs", AutoUpgrade{Enabled: true, Verdict: autoupgrade.Egress}, "Auto-upgrade waits until no job of a class with an egress allowlist runs."},
		{"installing", AutoUpgrade{Enabled: true, Verdict: autoupgrade.Go}, "Auto-upgrade is installing it now."},
		{"a new major version", AutoUpgrade{Enabled: true, Verdict: autoupgrade.Major}, "It is a new major version, which auto-upgrade leaves to you. Upgrade this agent with: sudo boxrunner upgrade"},
		{"a development build", AutoUpgrade{Enabled: true, Verdict: autoupgrade.Unreleased}, "This agent is a development build, which auto-upgrade leaves alone. Upgrade this agent with: sudo boxrunner upgrade"},
		{"turned off", AutoUpgrade{}, "Auto-upgrade is off. Upgrade this agent with: sudo boxrunner upgrade"},
	}
	for _, c := range cases {
		var b strings.Builder
		Print(&b, Snapshot{Box: "box-1", Version: "0.2.1", Worker: "0.2.2", Latest: "0.2.2", AutoUpgrade: c.auto}, t0)
		wantOn := "auto-upgrade " + map[bool]string{true: "on", false: "off"}[c.auto.Enabled] + "\n\nboxrunner 0.2.2 is out. " + c.want + "\n\n"
		if got := b.String(); !strings.Contains(got, wantOn) {
			t.Errorf("%s: Print =\n%s\nwant it to contain\n%s", c.name, got, wantOn)
		}
	}
	var b strings.Builder
	Print(&b, Snapshot{Box: "box-1", Version: "0.2.2", Worker: "0.2.2", Latest: "0.2.2", AutoUpgrade: AutoUpgrade{Enabled: true}}, t0)
	if got := b.String(); strings.Contains(got, "is out") || !strings.Contains(got, "  auto-upgrade on\n") {
		t.Errorf("the latest release is installed, Print =\n%s", got)
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

func TestPrintEgress(t *testing.T) {
	snap := Snapshot{
		Box: "box-1", Version: "0.1.0", DiskFloor: 10_000_000_000,
		Classes: []Class{{Name: "agent", Priority: "normal", Max: 1}, {Name: "web", Priority: "normal", Max: 1}},
		Egress: []Egress{
			{
				Class: "agent", Entries: []string{"github", "api.example.com", "10.20.0.0/16:5432"}, Packets: 12, Lookups: 1,
				Recent: []egress.Block{
					{Time: t0.Add(time.Second), Kind: egress.Packet, Runner: "box-1-agent-ab12cd", Repo: "octo/site", Dest: "203.0.113.9:443", Name: "api.example.com"},
					{Time: t0, Kind: egress.Lookup, Runner: "box-1-agent-ab12cd", Repo: "octo/site", Dest: "data.evil.example"},
					{Time: t0.Add(2 * time.Second), Kind: egress.Packet, Dest: "198.51.100.1:22"},
				},
			},
			{Class: "coder", Entries: []string{"github"}, Problem: "nft is not installed; run sudo boxrunner install"},
		},
	}
	var b strings.Builder
	Print(&b, snap, t0)
	at := func(d time.Duration) string { return t0.Add(d).Local().Format("15:04:05") }
	want := "box-1  boxrunner 0.1.0  worker unknown  image none (latest none)  disk 0.0 GB free, floor 10.0 GB  auto-upgrade off\n\n" +
		"Class coder has an egress allowlist and is not served: nft is not installed; run sudo boxrunner install\n\n" +
		"CLASS  PRIORITY  RUNNERS  MAX  DASHBOARD\n" +
		"agent  normal    0        1    -\n" +
		"web    normal    0        1    -\n\n" +
		"EGRESS CLASS  ENTRIES  PACKETS DROPPED  LOOKUPS REFUSED\n" +
		"agent         3        12               1\n" +
		"coder         1        0                0\n\n" +
		"REFUSED AT  CLASS  RUNNER              REPOSITORY  WHAT\n" +
		at(0) + "    agent  box-1-agent-ab12cd  octo/site   lookup of data.evil.example\n" +
		at(time.Second) + "    agent  box-1-agent-ab12cd  octo/site   packet to 203.0.113.9:443 (api.example.com)\n" +
		at(2*time.Second) + "    agent  ?                   ?           packet to 198.51.100.1:22\n\n"
	if got := b.String(); !strings.HasPrefix(got, want) {
		t.Errorf("Print =\n%s\nwant it to start with\n%s", got, want)
	}
	if got, want := snap.Egress[0].Summary(), "12 packets dropped and 1 lookup refused since the agent started"; got != want {
		t.Errorf("Summary = %q, want %q", got, want)
	}
}

func TestPrintDestinations(t *testing.T) {
	snap := Snapshot{
		Box: "box-1", Version: "0.1.0",
		Classes: []Class{{Name: "agent", Priority: "normal", Max: 1}},
		Destinations: []seen.Repo{
			{Repo: "", Destinations: []seen.Destination{{Addr: "198.51.100.4", Port: 53, Proto: "udp", First: t0, Last: t0, Count: 1}}},
			{Repo: "octo/site", Destinations: []seen.Destination{
				{Addr: "203.0.113.9", Port: 443, Proto: "tcp", Name: "api.example.com", First: t0.Add(time.Hour), Last: t0.Add(time.Hour), Count: 12},
				{Addr: "203.0.113.20", Proto: "icmp", First: t0, Last: t0, Count: 2},
			}},
		},
	}
	var b strings.Builder
	Print(&b, snap, t0)
	at := func(d time.Duration) string { return t0.Add(d).Local().Format("Jan 02 15:04") }
	want := "CLASS  PRIORITY  RUNNERS  MAX  DASHBOARD\n" +
		"agent  normal    0        1    -\n\n" +
		"REPOSITORY  FIRST SEEN    CONNECTIONS  DESTINATION\n" +
		"?           " + at(0) + "  1            198.51.100.4:53 udp\n" +
		"octo/site   " + at(time.Hour) + "  12           203.0.113.9:443 (api.example.com)\n" +
		"octo/site   " + at(0) + "  2            203.0.113.20 icmp\n" +
		"Up to 10 destinations per repository, the newest first. A ? is a container that is not one of the agent's runners.\n\n"
	if got := b.String(); !strings.Contains(got, want) {
		t.Errorf("Print =\n%s\nwant it to contain\n%s", got, want)
	}

	snap.Destinations, snap.DestinationsProblem = nil, "nft is not installed, so the destinations jobs connect to are not recorded; run sudo boxrunner install"
	b.Reset()
	Print(&b, snap, t0)
	if got, want := b.String(), "Destinations: nft is not installed, so the destinations jobs connect to are not recorded; run sudo boxrunner install\n\nCLASS"; !strings.Contains(got, want) {
		t.Errorf("Print =\n%s\nwant it to contain\n%s", got, want)
	}
}

func TestPrintOrganization(t *testing.T) {
	snap := Snapshot{
		Box: "box-1", Version: "0.1.0", DiskFloor: 10e9,
		Organization: &Organization{Name: "octo", Group: "boxrunner"},
		ScaleSets:    []ScaleSet{{Repo: "octo", Class: "boxrunner", Assigned: 1, Refused: map[string]int{"octo/secret": 1, "octo/docs": 2}}},
	}
	var b strings.Builder
	Print(&b, snap, t0)
	want := `The Worker serves the organization octo through one scale set per class in the runner group "boxrunner".

REPOSITORY           CLASS      ASSIGNED JOBS
octo (organization)  boxrunner  1

octo boxrunner was offered 2 jobs of octo/docs, which this box does not serve, so they wait at GitHub. Take the repository out of the runner group, or let class boxrunner serve it.
octo boxrunner was offered 1 job of octo/secret, which this box does not serve, so it waits at GitHub. Take the repository out of the runner group, or let class boxrunner serve it.
`
	if got := b.String(); !strings.Contains(got, want) {
		t.Errorf("Print =\n%s\nwant it to contain\n%s", got, want)
	}

	snap.Organization.Problem = "The runner group “boxrunner” admits every repository of octo; set its repository access to selected repositories."
	b.Reset()
	Print(&b, snap, t0)
	if want := "The Worker serves no repository of the organization octo: The runner group “boxrunner” admits every repository of octo; set its repository access to selected repositories.\n"; !strings.Contains(b.String(), want) {
		t.Errorf("Print =\n%s\nwant it to contain\n%s", b.String(), want)
	}
}

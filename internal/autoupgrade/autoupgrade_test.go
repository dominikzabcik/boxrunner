package autoupgrade

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func seenAgo(version string, ago time.Duration) State {
	return State{Version: version, Seen: now.Add(-ago)}
}

func TestDecide(t *testing.T) {
	ready := Input{Enabled: true, Current: "0.3.0", Latest: "0.3.1", State: seenAgo("0.3.1", 7*time.Hour), Now: now, Delay: Delay}
	with := func(change func(*Input)) Input {
		in := ready
		change(&in)
		return in
	}
	cases := []struct {
		name string
		in   Input
		want Decision
	}{
		{"a newer release seen long enough", ready, Decision{Verdict: Go, Version: "0.3.1"}},
		{"a newer minor release of the same major", with(func(in *Input) { in.Latest, in.State = "0.4.0", seenAgo("0.4.0", 24*time.Hour) }), Decision{Verdict: Go, Version: "0.4.0"}},
		{"just seen", with(func(in *Input) { in.State = seenAgo("0.3.1", 0) }), Decision{Verdict: Hold, Version: "0.3.1", Until: now.Add(6 * time.Hour)}},
		{"seen five hours ago", with(func(in *Input) { in.State = seenAgo("0.3.1", 5*time.Hour) }), Decision{Verdict: Hold, Version: "0.3.1", Until: now.Add(time.Hour)}},
		{"seen exactly six hours ago", with(func(in *Input) { in.State = seenAgo("0.3.1", 6*time.Hour) }), Decision{Verdict: Go, Version: "0.3.1"}},
		{"the state is about an older release", with(func(in *Input) { in.State = seenAgo("0.3.0", 48*time.Hour) }), Decision{Verdict: Hold, Version: "0.3.1", Until: now.Add(6 * time.Hour)}},
		{"no state yet", with(func(in *Input) { in.State = State{} }), Decision{Verdict: Hold, Version: "0.3.1", Until: now.Add(6 * time.Hour)}},
		{"turned off", with(func(in *Input) { in.Enabled = false }), Decision{Verdict: Off}},
		{"already the latest", with(func(in *Input) { in.Latest = "0.3.0" }), Decision{Verdict: Current}},
		{"a downgrade is never made", with(func(in *Input) { in.Latest = "0.2.9" }), Decision{Verdict: Current}},
		{"latest not found yet", with(func(in *Input) { in.Latest = "" }), Decision{Verdict: Current}},
		{"a newer major version", with(func(in *Input) { in.Latest, in.State = "1.0.0", seenAgo("1.0.0", 48*time.Hour) }), Decision{Verdict: Major, Version: "1.0.0"}},
		{"a development build", with(func(in *Input) { in.Current = "0.1.0-dev" }), Decision{Verdict: Unreleased}},
		{"a pre-release build", with(func(in *Input) { in.Current = "0.3.1-rc.1" }), Decision{Verdict: Unreleased}},
		{"draining", with(func(in *Input) { in.Draining = true }), Decision{Verdict: Draining, Version: "0.3.1"}},
		{"a job of an egress class runs", with(func(in *Input) { in.EgressJobs = 1 }), Decision{Verdict: Egress, Version: "0.3.1"}},
		{"draining and an egress job", with(func(in *Input) { in.Draining, in.EgressJobs = true, 2 }), Decision{Verdict: Draining, Version: "0.3.1"}},
		{"held by the delay while draining", with(func(in *Input) { in.Draining, in.State = true, seenAgo("0.3.1", time.Hour) }), Decision{Verdict: Hold, Version: "0.3.1", Until: now.Add(5 * time.Hour)}},
		{"an attempt half an hour ago", with(func(in *Input) {
			in.State.Attempts, in.State.LastAttempt = 1, now.Add(-30*time.Minute)
		}), Decision{Verdict: Retry, Version: "0.3.1", Until: now.Add(30 * time.Minute)}},
		{"an attempt an hour ago", with(func(in *Input) {
			in.State.Attempts, in.State.LastAttempt = 1, now.Add(-time.Hour)
		}), Decision{Verdict: Go, Version: "0.3.1"}},
		{"a third attempt waits four hours", with(func(in *Input) {
			in.State.Attempts, in.State.LastAttempt = 3, now.Add(-3*time.Hour)
		}), Decision{Verdict: Retry, Version: "0.3.1", Until: now.Add(time.Hour)}},
	}
	for _, c := range cases {
		if got := Decide(c.in); got != c.want {
			t.Errorf("%s: Decide = %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestNextAttempt(t *testing.T) {
	last := now
	for attempts, want := range map[int]time.Duration{1: time.Hour, 2: 2 * time.Hour, 3: 4 * time.Hour, 4: 8 * time.Hour, 5: 16 * time.Hour, 6: 24 * time.Hour, 40: 24 * time.Hour} {
		s := State{Version: "0.3.1", Attempts: attempts, LastAttempt: last}
		if got := s.NextAttempt().Sub(last); got != want {
			t.Errorf("after %d attempts, wait %v; want %v", attempts, got, want)
		}
	}
	if got := (State{}).NextAttempt(); !got.IsZero() {
		t.Errorf("without attempts, NextAttempt = %v", got)
	}
}

func TestObserve(t *testing.T) {
	seen := now.Add(-time.Hour)
	known := State{Version: "0.3.1", Seen: seen, Attempts: 2, LastAttempt: seen}
	if got := known.Observe("0.3.1", now); got != known {
		t.Errorf("Observe of the same release = %+v, want it unchanged", got)
	}
	if got, want := known.Observe("0.3.2", now), (State{Version: "0.3.2", Seen: now}); got != want {
		t.Errorf("Observe of a new release = %+v, want %+v", got, want)
	}
	future := State{Version: "0.3.1", Seen: now.Add(time.Hour)}
	if got, want := future.Observe("0.3.1", now), (State{Version: "0.3.1", Seen: now}); got != want {
		t.Errorf("Observe after the clock went back = %+v, want %+v", got, want)
	}
}

func TestStateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "release-seen.json")
	if got := Load(path); got != (State{}) {
		t.Errorf("Load of a missing file = %+v", got)
	}
	want := State{Version: "0.3.1", Seen: now, Attempts: 1, LastAttempt: now.Add(time.Minute)}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	if got := Load(path); got != want {
		t.Errorf("Load = %+v, want %+v", got, want)
	}
}

func TestCommand(t *testing.T) {
	name, args := Command("0.3.1")
	wantArgs := []string{
		"--unit=boxrunner-upgrade",
		"--description=boxrunner auto-upgrade to 0.3.1",
		"--collect",
		"--setenv=HOME=/root",
		"/usr/local/bin/boxrunner", "upgrade", "0.3.1",
	}
	if name != "systemd-run" || !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("Command = %s %q, want systemd-run %q", name, args, wantArgs)
	}
}

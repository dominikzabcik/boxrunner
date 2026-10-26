package agent

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/autoupgrade"
	"github.com/dominikzabcik/boxrunner/internal/config"
	"github.com/dominikzabcik/boxrunner/internal/egress"
	"github.com/dominikzabcik/boxrunner/internal/status"
)

const wantUpgradeCommand = "--unit=boxrunner-upgrade --description=boxrunner auto-upgrade to 0.2.2 --collect --setenv=HOME=/root /usr/local/bin/boxrunner upgrade 0.2.2"

func (h *harness) upgrades() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.systemdRun...)
}

func autoUpgradeStatus(t *testing.T, h *harness, verdict autoupgrade.Verdict) status.AutoUpgrade {
	t.Helper()
	var got status.AutoUpgrade
	waitFor(t, "auto-upgrade to decide "+string(verdict), func() bool {
		s, err := status.Fetch(context.Background(), h.socket)
		got = s.AutoUpgrade
		return err == nil && got.Verdict == verdict
	})
	return got
}

// The latest release (0.2.2) is newer than the agent (0.1.0) and was seen
// long enough ago: the agent starts the upgrade once, in a unit of its own,
// and then waits an hour before it would try again.
func TestAutoUpgradeStartsTheUpgrade(t *testing.T) {
	h := startAgent(t, map[string]*container{}, nil, func(_ *config.Class, h *harness) {
		h.autoUpgrade, h.upgradeDelay = true, time.Millisecond
	})
	defer h.stop(t)
	waitFor(t, "the upgrade command", func() bool { return len(h.upgrades()) > 0 })
	if got, want := h.upgrades(), []string{wantUpgradeCommand}; !reflect.DeepEqual(got, want) {
		t.Errorf("systemd-run calls = %q, want %q", got, want)
	}
	got := autoUpgradeStatus(t, h, autoupgrade.Retry)
	if !got.Enabled || got.Until.Before(time.Now().Add(50*time.Minute)) {
		t.Errorf("status after the attempt = %+v, want it on and retrying in about an hour", got)
	}
	time.Sleep(300 * time.Millisecond)
	if got := h.upgrades(); len(got) != 1 {
		t.Errorf("the agent started the upgrade %d times: %q", len(got), got)
	}
	state := autoupgrade.Load(h.seen)
	if state.Version != "0.2.2" || state.Attempts != 1 || state.Seen.IsZero() {
		t.Errorf("the state file = %+v, want one attempt at 0.2.2", state)
	}
}

// The wait counts from when the agent first saw the release, and a restart
// keeps it: the state file says it was seen an hour ago.
func TestAutoUpgradeHolds(t *testing.T) {
	h := startAgent(t, map[string]*container{}, nil, func(_ *config.Class, h *harness) {
		h.autoUpgrade = true
		seen := time.Now().Add(-time.Hour).Truncate(time.Second)
		if err := autoupgrade.Save(h.seen, autoupgrade.State{Version: "0.2.2", Seen: seen}); err != nil {
			t.Fatal(err)
		}
	})
	defer h.stop(t)
	got := autoUpgradeStatus(t, h, autoupgrade.Hold)
	if want := autoupgrade.Load(h.seen).Seen.Add(autoupgrade.Delay); !got.Until.Equal(want) || time.Until(want) < 4*time.Hour {
		t.Errorf("held until %v, want %v, five hours away", got.Until, want)
	}
	if calls := h.upgrades(); len(calls) != 0 {
		t.Errorf("the agent upgraded inside the delay: %q", calls)
	}
}

// A restart after the delay went by does not make the agent wait again.
func TestAutoUpgradeAfterARestart(t *testing.T) {
	h := startAgent(t, map[string]*container{}, nil, func(_ *config.Class, h *harness) {
		h.autoUpgrade = true
		autoupgrade.Save(h.seen, autoupgrade.State{Version: "0.2.2", Seen: time.Now().Add(-7 * time.Hour)})
	})
	defer h.stop(t)
	waitFor(t, "the upgrade command", func() bool { return len(h.upgrades()) > 0 })
	if got, want := h.upgrades(), []string{wantUpgradeCommand}; !reflect.DeepEqual(got, want) {
		t.Errorf("systemd-run calls = %q, want %q", got, want)
	}
}

func TestAutoUpgradeOffNeverUpgrades(t *testing.T) {
	h := startAgent(t, map[string]*container{}, nil, func(_ *config.Class, h *harness) { h.upgradeDelay = time.Millisecond })
	defer h.stop(t)
	got := autoUpgradeStatus(t, h, autoupgrade.Off)
	if got.Enabled {
		t.Errorf("status = %+v, want auto-upgrade off", got)
	}
	time.Sleep(300 * time.Millisecond)
	if calls := h.upgrades(); len(calls) != 0 {
		t.Errorf("the agent upgraded with auto-upgrade off: %q", calls)
	}
}

func TestAutoUpgradeWaitsWhileDraining(t *testing.T) {
	h := startAgent(t, map[string]*container{}, nil, func(_ *config.Class, h *harness) {
		h.autoUpgrade, h.upgradeDelay = true, time.Millisecond
		if err := os.WriteFile(h.drain, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	})
	defer h.stop(t)
	autoUpgradeStatus(t, h, autoupgrade.Draining)
	time.Sleep(300 * time.Millisecond)
	if calls := h.upgrades(); len(calls) != 0 {
		t.Errorf("a draining box upgraded: %q", calls)
	}
}

// Stopping the agent stops the running jobs of classes with an egress
// allowlist; a job of any other class, and a runner without a job, do not count.
func TestEgressJobs(t *testing.T) {
	policy, _ := egress.Parse([]string{"github"})
	a := &Agent{Options: Options{Config: config.Config{Classes: []config.Class{
		{Name: "agent", Egress: &policy},
		{Name: "plain"},
	}}}}
	snap := status.Snapshot{Runners: []status.Runner{
		{Name: "box-1-agent-aaaaaa", Class: "agent", Phase: "running"},
		{Name: "box-1-agent-bbbbbb", Class: "agent", Phase: "starting", Warm: true},
		{Name: "box-1-agent-cccccc", Class: "agent", Phase: "exited"},
		{Name: "box-1-plain-dddddd", Class: "plain", Phase: "running"},
		{Name: "box-1-gone-eeeeee", Class: "gone", Phase: "running"},
	}}
	got := a.egressJobs(snap)
	if len(got) != 1 || got[0].Name != "box-1-agent-aaaaaa" {
		t.Errorf("egressJobs = %+v, want only box-1-agent-aaaaaa", got)
	}
}

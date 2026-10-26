package agent

import (
	"cmp"
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/autoupgrade"
	"github.com/dominikzabcik/boxrunner/internal/layout"
	"github.com/dominikzabcik/boxrunner/internal/release"
	"github.com/dominikzabcik/boxrunner/internal/status"
)

const (
	// releaseEvery is how often boxrunner's latest release is looked up. The
	// lookup is one request to github.com's release page; asking often
	// shortens how long a box runs a release that a fix has already replaced.
	releaseEvery = time.Hour
	// upgradeEvery is how often the agent weighs an upgrade, so that one held
	// back by a running job or the delay goes ahead soon after the reason ends.
	upgradeEvery = 5 * time.Minute
)

// checkRelease looks for boxrunner's latest release, so status and the
// dashboard can say when a newer one is out, and installs it when the box
// allows that and the release has been out long enough.
func (a *Agent) checkRelease(ctx context.Context) {
	client := &http.Client{Timeout: 30 * time.Second}
	delay := cmp.Or(a.UpgradeDelay, autoupgrade.Delay)
	every := cmp.Or(a.UpgradeEvery, upgradeEvery)
	seenFile := cmp.Or(a.ReleaseSeenFile, layout.ReleaseSeenPath)
	state := autoupgrade.Load(seenFile)
	var fetchAt time.Time
	var logged string
	for {
		if now := time.Now(); !now.Before(fetchAt) {
			a.fetchLatest(ctx, client)
			fetchAt = now.Add(releaseEvery)
		}
		if latest := a.latest.Load(); latest != nil {
			state, logged = a.considerUpgrade(ctx, client, *latest, state, delay, seenFile, logged)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

func (a *Agent) fetchLatest(ctx context.Context, client *http.Client) {
	latest, err := release.Latest(ctx, client, a.BoxrunnerReleasesURL)
	if err != nil {
		a.Log.Warn("checking boxrunner's latest release", "err", err)
		return
	}
	a.latest.Store(&latest)
}

// considerUpgrade weighs the latest release against the box's state and
// starts the upgrade when it is due. It returns the state to keep and a key
// for what it last logged, so that a wait is logged once, not every five minutes.
func (a *Agent) considerUpgrade(ctx context.Context, client *http.Client, latest string, state autoupgrade.State, delay time.Duration, seenFile, logged string) (autoupgrade.State, string) {
	now := time.Now()
	if seen := state.Observe(latest, now); seen != state {
		state = seen
		if err := autoupgrade.Save(seenFile, state); err != nil {
			a.Log.Warn("saving when the latest release was first seen", "err", err)
		}
	}
	snap := a.current()
	d := autoupgrade.Decide(autoupgrade.Input{
		Enabled: a.Config.AutoUpgrade, Current: a.Version, Latest: latest, State: state, Now: now, Delay: delay,
		Draining: a.draining.Load(), EgressJobs: len(a.egressJobs(snap)),
	})
	a.upgrade.Store(&status.AutoUpgrade{Enabled: a.Config.AutoUpgrade, Verdict: d.Verdict, Until: d.Until})

	if key := string(d.Verdict) + " " + d.Version; key != logged && d.Verdict != autoupgrade.Current {
		logged = key
		log := a.Log.With("version", a.Version, "latest", latest)
		switch d.Verdict {
		case autoupgrade.Off, autoupgrade.Unreleased:
			log.Info("a newer boxrunner is out; upgrade with: sudo boxrunner upgrade")
		case autoupgrade.Major:
			log.Info("a new major version of boxrunner is out; upgrade with: sudo boxrunner upgrade")
		case autoupgrade.Hold, autoupgrade.Retry:
			log.Info("a newer boxrunner is out; auto-upgrade installs it later", "after", d.Until.Format(time.RFC3339))
		case autoupgrade.Draining:
			log.Info("a newer boxrunner is out; auto-upgrade waits until the box resumes")
		case autoupgrade.Egress:
			log.Info("a newer boxrunner is out; auto-upgrade waits until no job of a class with an egress allowlist runs")
		}
	}
	if d.Verdict != autoupgrade.Go {
		return state, logged
	}

	// The release may have been replaced since the last lookup.
	if fresh, err := release.Latest(ctx, client, a.BoxrunnerReleasesURL); err != nil || fresh != latest {
		if err == nil {
			a.latest.Store(&fresh)
		}
		return state, logged
	}
	// The attempt counts before it starts, so a crash cannot loop on it.
	state = state.Attempted(now)
	if err := autoupgrade.Save(seenFile, state); err != nil {
		a.Log.Warn("saving the auto-upgrade attempt; not upgrading", "err", err)
		return state, logged
	}
	name, args := autoupgrade.Command(d.Version)
	a.Log.Info("upgrading boxrunner; follow it with: journalctl -u "+layout.UpgradeUnit, "version", a.Version, "target", d.Version)
	if out, err := a.Run(ctx, "", name, args...); err != nil {
		a.Log.Warn("starting the upgrade failed; trying again later", "err", err, "output", strings.TrimSpace(out), "after", state.NextAttempt().Format(time.RFC3339))
	}
	return state, logged
}

// egressJobs are the running jobs of classes with an egress allowlist, which
// stopping the agent stops. A runner that has not taken a job is not counted:
// shutdown removes it, and a warm one would hold the upgrade back forever.
func (a *Agent) egressJobs(snap status.Snapshot) []status.Runner {
	var jobs []status.Runner
	for _, r := range snap.Runners {
		if c, ok := a.Config.Class(r.Class); ok && c.Egress != nil && r.Phase == "running" {
			jobs = append(jobs, r)
		}
	}
	return jobs
}

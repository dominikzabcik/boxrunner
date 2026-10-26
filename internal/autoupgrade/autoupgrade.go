// Package autoupgrade decides when the agent installs a newer boxrunner
// release on its own, and builds the command that does it. The install itself
// is `boxrunner upgrade`, run in a unit of its own: the installer restarts the
// agent's service, and systemd would kill an installer that ran inside it.
package autoupgrade

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/layout"
	"github.com/dominikzabcik/boxrunner/internal/release"
)

// Delay is how long the agent has seen a release before it installs it, so a
// release that turns out broken is pulled or fixed before most boxes take it.
// It counts from when this agent first saw the release, which can be an hour
// after it came out.
const Delay = 6 * time.Hour

const (
	firstRetry = time.Hour
	lastRetry  = 24 * time.Hour
)

// Verdict is what to do about the latest release.
type Verdict string

const (
	// Go means install it now.
	Go Verdict = "go"
	// Off means the box turned auto-upgrade off.
	Off Verdict = "off"
	// Current means no newer release is out.
	Current Verdict = "current"
	// Unreleased means this agent is a development or pre-release build, which
	// is never replaced on its own.
	Unreleased Verdict = "unreleased"
	// Major means the latest release is of a newer major version, which can
	// change what the box needs; a person upgrades to it.
	Major Verdict = "major"
	// Hold means the release has not been seen for Delay yet; Until is when it has.
	Hold Verdict = "hold"
	// Retry means an earlier attempt ran not long ago; Until is the next.
	Retry Verdict = "retry"
	// Draining means the box drains.
	Draining Verdict = "draining"
	// Egress means a job of a class with an egress allowlist runs, which the
	// restart would stop.
	Egress Verdict = "egress"
)

var releasePattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// Input is what Decide looks at.
type Input struct {
	Enabled bool
	// Current is this agent's version and Latest the newest release.
	Current, Latest string
	// State holds when Latest was first seen and the attempts at installing it.
	State State
	Now   time.Time
	// Delay is how long Latest must have been seen, Delay unless a test says otherwise.
	Delay time.Duration
	// Draining is whether the box drains, and EgressJobs the number of jobs
	// running in classes with an egress allowlist.
	Draining   bool
	EgressJobs int
}

// Decision is Decide's answer.
type Decision struct {
	Verdict Verdict
	// Version is the release to install, set unless the verdict is Off,
	// Current or Unreleased.
	Version string
	// Until is when a Hold or Retry ends.
	Until time.Time
}

// Decide says whether to install Latest now, and if not, what stands in the
// way. It never picks an older release or one of another major version.
func Decide(in Input) Decision {
	switch {
	case !in.Enabled:
		return Decision{Verdict: Off}
	case !release.Newer(in.Latest, in.Current):
		return Decision{Verdict: Current}
	case !releasePattern.MatchString(in.Current):
		return Decision{Verdict: Unreleased}
	}
	d := Decision{Version: in.Latest}
	if release.Major(in.Latest) != release.Major(in.Current) {
		d.Verdict = Major
		return d
	}
	s := in.State
	if s.Version != in.Latest {
		s = State{Version: in.Latest, Seen: in.Now}
	}
	if until := s.Seen.Add(in.Delay); in.Now.Before(until) {
		d.Verdict, d.Until = Hold, until
		return d
	}
	if until := s.NextAttempt(); in.Now.Before(until) {
		d.Verdict, d.Until = Retry, until
		return d
	}
	switch {
	case in.Draining:
		d.Verdict = Draining
	case in.EgressJobs > 0:
		d.Verdict = Egress
	default:
		d.Verdict = Go
	}
	return d
}

// State is kept in a file, so that neither a restart of the agent nor the
// failure of an attempt starts the wait over.
type State struct {
	// Version is the release the rest is about.
	Version string `json:"version"`
	// Seen is when the agent first saw it as the latest.
	Seen time.Time `json:"seen"`
	// Attempts counts the times the agent started its installation, and
	// LastAttempt is the latest.
	Attempts    int       `json:"attempts,omitempty"`
	LastAttempt time.Time `json:"lastAttempt,omitzero"`
}

// Observe returns the state after the latest release was seen at now. A
// release the state does not know starts it afresh.
func (s State) Observe(latest string, now time.Time) State {
	if s.Version != latest || s.Seen.After(now) {
		return State{Version: latest, Seen: now}
	}
	return s
}

// NextAttempt is the earliest time to try again: an hour after the first
// attempt, then twice as long each time, up to a day. The zero Time means no
// attempt yet.
func (s State) NextAttempt() time.Time {
	if s.Attempts == 0 {
		return time.Time{}
	}
	wait := lastRetry
	if s.Attempts <= 5 {
		wait = min(firstRetry<<(s.Attempts-1), lastRetry)
	}
	return s.LastAttempt.Add(wait)
}

// Attempted returns the state after an attempt at now.
func (s State) Attempted(now time.Time) State {
	s.Attempts++
	s.LastAttempt = now
	return s
}

// Load reads the state file. A missing or unreadable file gives the zero
// State, which counts as having seen nothing.
func Load(path string) State {
	var s State
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, &s)
	}
	return s
}

// Save writes the state file whole or not at all.
func Save(path string, s State) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".release-seen-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(append(data, '\n'))
	err = errors.Join(err, tmp.Chmod(0o644), tmp.Close())
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Command is the command that installs a release outside the agent's own
// systemd unit. systemd-run starts a transient service and returns; the
// service survives the restart that the installer makes. Its output goes to
// the journal under layout.UpgradeUnit. HOME is set so gh finds root's login
// and the upgrade checks the installer's attestation, as `sudo boxrunner
// upgrade` does.
func Command(version string) (name string, args []string) {
	return "systemd-run", []string{
		"--unit=" + layout.UpgradeUnit,
		"--description=boxrunner auto-upgrade to " + version,
		"--collect",
		"--setenv=HOME=/root",
		layout.Binary, "upgrade", version,
	}
}

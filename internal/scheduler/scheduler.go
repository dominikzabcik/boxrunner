// Package scheduler decides which runners to start and remove. State is
// changed only by Apply and read only by Decide; both are pure, and one
// goroutine in the agent owns the State.
package scheduler

import (
	"cmp"
	"maps"
	"slices"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/config"
	"github.com/dominikzabcik/boxrunner/internal/queue"
)

// IdleTimeout is how long a started runner may wait for a job before it is
// removed.
const IdleTimeout = 3 * time.Minute

// BuildRetry is how long a failed image build waits before the next attempt.
const BuildRetry = time.Hour

// Key names a scale set: one repository and one class.
type Key struct {
	Repo  string
	Class string
}

type Phase int

const (
	// Requested: a runner config is being minted and its container created.
	Requested Phase = iota
	// Starting: the container runs and the runner has not taken a job yet.
	Starting
	// Running: the runner took a job.
	Running
	// Removing: the agent is removing the runner and its container.
	Removing
	// Exited: the container stopped or the job completed; it still needs removing.
	Exited
)

var phaseNames = [...]string{"requested", "starting", "running", "removing", "exited"}

func (p Phase) String() string { return phaseNames[p] }

type Runner struct {
	Name       string
	ScaleSet   Key
	ScaleSetID int64
	// ID is the runner's id at GitHub, 0 until its config is minted.
	ID      int64
	Phase   Phase
	Since   time.Time
	TookJob bool
	// Disk is what the container wrote, in bytes.
	Disk int64
}

func (r Runner) holdsJob() bool {
	return r.Phase == Requested || r.Phase == Starting || r.Phase == Running
}

type ScaleSet struct {
	ID int64
	// Jobs is the queue time of every job assigned to the scale set, oldest
	// first, including the ones runners already took.
	Jobs []time.Time
}

type Class struct {
	Max      int
	Priority config.Priority
}

type Image struct {
	Built    string
	Latest   string
	Building bool
	RetryAt  time.Time
}

type State struct {
	Classes   map[string]Class
	ScaleSets map[Key]ScaleSet
	Runners   map[string]Runner
	DiskFloor int64
	DiskFree  int64
	// Observed is false until the first look at Docker; nothing is decided before.
	Observed bool
	Image    Image
}

func New(cfg config.Config) State {
	s := State{
		Classes:   map[string]Class{},
		ScaleSets: map[Key]ScaleSet{},
		Runners:   map[string]Runner{},
		DiskFloor: int64(cfg.DiskFloor),
	}
	for _, c := range cfg.Classes {
		s.Classes[c.Name] = Class{Max: c.Max, Priority: c.Priority}
	}
	return s
}

// Actions.

type Action interface{ action() }

// Start asks for a new runner in a scale set.
type Start struct{ ScaleSet Key }

// Remove removes a runner's container; with Deregister it first removes the
// runner at GitHub, which refuses when the runner already took a job.
type Remove struct {
	Runner     string
	Deregister bool
	Reason     string
}

// Build rebuilds the job image for a runner version.
type Build struct{ Version string }

func (Start) action()  {}
func (Remove) action() {}
func (Build) action()  {}

// Decide returns what to do next. Removals come first, then starts, then a build.
func Decide(s State, now time.Time) []Action {
	if !s.Observed {
		return nil
	}
	var actions []Action
	removing := false
	for _, r := range sortedRunners(s) {
		switch {
		case r.Phase == Removing:
			removing = true
		case r.Phase == Exited:
			actions = append(actions, Remove{Runner: r.Name, Deregister: !r.TookJob, Reason: "exited"})
		case r.Phase == Starting && now.Sub(r.Since) >= IdleTimeout:
			actions = append(actions, Remove{Runner: r.Name, Deregister: true, Reason: "took no job within 3 minutes"})
		}
	}

	if !removing && s.DiskFree < s.DiskFloor/2 {
		var largest *Runner
		for _, r := range sortedRunners(s) {
			if r.Phase == Running && (largest == nil || r.Disk > largest.Disk) {
				largest = &r
			}
		}
		if largest != nil {
			actions = append(actions, Remove{Runner: largest.Name, Reason: "free disk below half the floor"})
		}
	}

	if s.DiskFree >= s.DiskFloor {
		actions = append(actions, starts(s)...)
	}

	if img := s.Image; img.Latest != "" && img.Latest != img.Built && !img.Building && !now.Before(img.RetryAt) && !normalBusy(s) {
		actions = append(actions, Build{Version: img.Latest})
	}
	return actions
}

type waiting struct {
	queued time.Time
	key    Key
}

func starts(s State) []Action {
	var actions []Action
	for _, class := range slices.Sorted(maps.Keys(s.Classes)) {
		active := 0
		held := map[Key]int{}
		for _, r := range s.Runners {
			if r.ScaleSet.Class != class {
				continue
			}
			if r.Phase != Exited {
				active++
			}
			if r.holdsJob() {
				held[r.ScaleSet]++
			}
		}
		room := s.Classes[class].Max - active
		if room <= 0 {
			continue
		}
		// Runners take a scale set's oldest jobs first, so its newest ones wait.
		var queue []waiting
		for key, ss := range s.ScaleSets {
			if key.Class != class {
				continue
			}
			for _, t := range ss.Jobs[min(held[key], len(ss.Jobs)):] {
				queue = append(queue, waiting{t, key})
			}
		}
		slices.SortFunc(queue, func(a, b waiting) int {
			return cmp.Or(a.queued.Compare(b.queued), cmp.Compare(a.key.Repo, b.key.Repo))
		})
		for _, w := range queue[:min(room, len(queue))] {
			actions = append(actions, Start{ScaleSet: w.key})
		}
	}
	return actions
}

func normalBusy(s State) bool {
	for _, r := range s.Runners {
		if c, ok := s.Classes[r.ScaleSet.Class]; ok && c.Priority == config.Normal && r.holdsJob() {
			return true
		}
	}
	return false
}

func sortedRunners(s State) []Runner {
	return slices.SortedFunc(maps.Values(s.Runners), func(a, b Runner) int { return cmp.Compare(a.Name, b.Name) })
}

// Events.

type Event interface{ event() }

// Update is what one queue message says about a scale set. It is one event,
// so no decision falls between a job completing and its scale set's count
// going down.
type Update struct {
	ScaleSet Key
	ID       int64
	// Jobs replaces the scale set's assigned jobs.
	Jobs []time.Time
	// Started names the runners that took a job.
	Started []string
	// Completed carries the finished jobs, also for the job log.
	Completed []queue.JobCompleted
}

// ScaleSetGone means the agent stopped listening to a scale set.
type ScaleSetGone struct{ ScaleSet Key }

// Container is one labelled container as Docker lists it.
type Container struct {
	Runner     string
	ScaleSet   Key
	ScaleSetID int64
	RunnerID   int64
	Running    bool
	Created    time.Time
	Disk       int64
}

// Observed is a fresh look at Docker: every labelled container and free disk.
type Observed struct {
	Containers []Container
	DiskFree   int64
}

// StartDispatched records a Start the agent dispatched under a runner name.
type StartDispatched struct {
	Runner   string
	ScaleSet Key
}

// RemoveDispatched records a Remove the agent dispatched.
type RemoveDispatched struct{ Runner string }

type BuildDispatched struct{}

type Started struct {
	Runner string
	ID     int64
}

type StartFailed struct{ Runner string }

type Removed struct{ Runner string }

// Busy means GitHub refused to remove the runner because it took a job.
type Busy struct{ Runner string }

type ImageBuilt struct{ Version string }

type BuildFailed struct{}

type LatestRunner struct{ Version string }

func (Update) event()           {}
func (ScaleSetGone) event()     {}
func (Observed) event()         {}
func (StartDispatched) event()  {}
func (RemoveDispatched) event() {}
func (BuildDispatched) event()  {}
func (Started) event()          {}
func (StartFailed) event()      {}
func (Removed) event()          {}
func (Busy) event()             {}
func (ImageBuilt) event()       {}
func (BuildFailed) event()      {}
func (LatestRunner) event()     {}

// Apply returns the state after an event. It never changes s.
func Apply(s State, e Event, now time.Time) State {
	s.Runners = maps.Clone(s.Runners)
	s.ScaleSets = maps.Clone(s.ScaleSets)
	update := func(name string, f func(*Runner)) {
		if r, ok := s.Runners[name]; ok {
			f(&r)
			s.Runners[name] = r
		}
	}
	switch e := e.(type) {
	case Update:
		for _, name := range e.Started {
			update(name, func(r *Runner) {
				if r.Phase == Requested || r.Phase == Starting {
					r.Phase, r.Since = Running, now
				}
				r.TookJob = true
			})
		}
		for _, j := range e.Completed {
			update(j.RunnerName, func(r *Runner) {
				if r.Phase != Removing {
					r.Phase, r.Since = Exited, now
				}
				r.TookJob = true
			})
		}
		s.ScaleSets[e.ScaleSet] = ScaleSet{ID: e.ID, Jobs: e.Jobs}
	case ScaleSetGone:
		delete(s.ScaleSets, e.ScaleSet)
	case Observed:
		s.Observed = true
		s.DiskFree = e.DiskFree
		s.Runners = observe(s.Runners, e.Containers, now)
	case StartDispatched:
		s.Runners[e.Runner] = Runner{
			Name: e.Runner, ScaleSet: e.ScaleSet, ScaleSetID: s.ScaleSets[e.ScaleSet].ID,
			Phase: Requested, Since: now,
		}
	case RemoveDispatched:
		update(e.Runner, func(r *Runner) { r.Phase, r.Since = Removing, now })
	case BuildDispatched:
		s.Image.Building = true
	case Started:
		update(e.Runner, func(r *Runner) {
			r.ID = e.ID
			if r.Phase == Requested {
				r.Phase, r.Since = Starting, now
			}
		})
	case StartFailed:
		delete(s.Runners, e.Runner)
	case Removed:
		delete(s.Runners, e.Runner)
	case Busy:
		update(e.Runner, func(r *Runner) { r.Phase, r.Since, r.TookJob = Running, now, true })
	case ImageBuilt:
		s.Image.Built, s.Image.Building = e.Version, false
	case BuildFailed:
		s.Image.Building, s.Image.RetryAt = false, now.Add(BuildRetry)
	case LatestRunner:
		s.Image.Latest = e.Version
	default:
		panic("scheduler: unhandled event")
	}
	return s
}

func observe(runners map[string]Runner, containers []Container, now time.Time) map[string]Runner {
	seen := map[string]bool{}
	for _, c := range containers {
		seen[c.Runner] = true
		r, known := runners[c.Runner]
		if !known {
			// A container from before a restart. It counts as starting from its
			// creation, so one that never took a job is removed in time, and
			// GitHub's refusal to remove a busy runner marks it running.
			r = Runner{Name: c.Runner, ScaleSet: c.ScaleSet, ScaleSetID: c.ScaleSetID, ID: c.RunnerID, Phase: Starting, Since: c.Created}
			if !c.Running {
				r.Phase = Exited
			}
		} else if !c.Running && (r.Phase == Starting || r.Phase == Running) {
			r.Phase, r.Since = Exited, now
		}
		r.Disk = c.Disk
		runners[c.Runner] = r
	}
	for name, r := range runners {
		if !seen[name] && (r.Phase == Starting || r.Phase == Running) {
			r.Phase, r.Since = Exited, now
			runners[name] = r
		}
	}
	return runners
}

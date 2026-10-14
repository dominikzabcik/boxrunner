// Package scheduler decides which runners to start and remove. State is
// changed only by Apply and read only by Decide; both are pure, and one
// goroutine in the agent owns the State.
package scheduler

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/config"
	"github.com/dominikzabcik/boxrunner/internal/queue"
)

// IdleTimeout is how long a started runner may wait for a job before it is
// removed. A warm runner waits as long as it takes.
const IdleTimeout = 3 * time.Minute

// ResultWait is how long a stopped container of a class that keeps failed
// jobs waits for the job's result before it is removed.
const ResultWait = 30 * time.Second

// BuildRetry is how long a failed image build waits before the next attempt.
const BuildRetry = time.Hour

// After failuresBeforeHold runners of a class in a row fail before taking a
// job, the class holds its starts for a minute, then twice as long after each
// further failure, up to maxHold.
const (
	failuresBeforeHold = 3
	maxHold            = 30 * time.Minute
)

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
	// Result is GitHub's result for the runner's job, once it completed.
	Result string
	// KeepUntil holds an exited container for inspection until then.
	KeepUntil time.Time
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
	// KeepFailed holds a failed job's container this long before it is removed.
	KeepFailed time.Duration
	// Warm names the repositories, lower case, that keep one idle runner ready.
	Warm []string
	// MaxDuration stops a runner whose job runs longer; 0 never does.
	MaxDuration time.Duration
}

func (c Class) warm(repo string) bool { return slices.Contains(c.Warm, strings.ToLower(repo)) }

// Backoff is a class whose runners fail before they take a job: their
// containers exit, or fail to start.
type Backoff struct {
	// Failures counts the failures in a row.
	Failures int
	// Until is when the class may start a runner again; zero until it holds.
	Until time.Time
	// Runner is the runner whose failure set Until.
	Runner string
}

func (b Backoff) holding() bool { return b.Failures >= failuresBeforeHold }

func hold(failures int) time.Duration {
	if n := failures - failuresBeforeHold; n < 5 {
		return min(time.Minute<<n, maxHold)
	}
	return maxHold
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
	// HeldBy names the other box whose session holds each scale set this box
	// could not open.
	HeldBy    map[Key]string
	Runners   map[string]Runner
	DiskFloor int64
	DiskFree  int64
	// Observed is false until the first look at Docker; nothing is decided before.
	Observed bool
	Image    Image
	// Backoff holds the classes whose runners fail before a job, by class.
	Backoff map[string]Backoff
}

func New(cfg config.Config) State {
	s := State{
		Classes:   map[string]Class{},
		ScaleSets: map[Key]ScaleSet{},
		HeldBy:    map[Key]string{},
		Runners:   map[string]Runner{},
		DiskFloor: int64(cfg.DiskFloor),
		Backoff:   map[string]Backoff{},
	}
	for _, c := range cfg.Classes {
		s.Classes[c.Name] = Class{Max: c.Max, Priority: c.Priority, KeepFailed: c.KeepFailed, Warm: c.Warm, MaxDuration: c.MaxDuration}
	}
	return s
}

// Actions.

type Action interface{ action() }

// Start asks for a new runner in a scale set.
type Start struct{ ScaleSet Key }

// Remove removes a runner's container; with Deregister it first removes the
// runner at GitHub, which refuses when the runner already took a job. With
// Logs, its container's last lines are logged first: its exit made its class
// hold.
type Remove struct {
	Runner     string
	Deregister bool
	Reason     string
	Logs       bool
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
	warm := Warm(s)
	removing := false
	for _, r := range sortedRunners(s) {
		maxDuration := s.Classes[r.ScaleSet.Class].MaxDuration
		switch {
		case r.Phase == Removing:
			removing = true
		case r.Phase == Exited && now.Before(r.KeepUntil):
		case r.Phase == Exited && r.TookJob && r.Result == "" && s.Classes[r.ScaleSet.Class].KeepFailed > 0 && now.Sub(r.Since) < ResultWait:
			// The container stopped before GitHub's message said how the job ended; a failed one is kept.
		case r.Phase == Exited:
			logs := !r.TookJob && s.Backoff[r.ScaleSet.Class].Runner == r.Name
			actions = append(actions, Remove{Runner: r.Name, Deregister: !r.TookJob, Reason: "exited", Logs: logs})
		case r.Phase == Starting && !warm[r.Name] && now.Sub(r.Since) >= IdleTimeout:
			actions = append(actions, Remove{Runner: r.Name, Deregister: true, Reason: "took no job within 3 minutes"})
		case r.Phase == Running && maxDuration > 0 && now.Sub(r.Since) >= maxDuration:
			actions = append(actions, Remove{Runner: r.Name, Reason: fmt.Sprintf("job ran longer than max_duration (%s)", maxDuration)})
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
		actions = append(actions, starts(s, now)...)
	}

	if img := s.Image; img.Latest != "" && img.Latest != img.Built && !img.Building && !now.Before(img.RetryAt) && !normalBusy(s, warm) {
		actions = append(actions, Build{Version: img.Latest})
	}
	return actions
}

type waiting struct {
	queued time.Time
	key    Key
}

func starts(s State, now time.Time) []Action {
	var actions []Action
	for _, class := range slices.Sorted(maps.Keys(s.Classes)) {
		backoff := s.Backoff[class]
		if backoff.holding() && now.Before(backoff.Until) {
			continue
		}
		active, pending := 0, 0
		held := map[Key]int{}
		for _, r := range s.Runners {
			if r.ScaleSet.Class != class {
				continue
			}
			if r.Phase != Exited {
				active++
			}
			if r.Phase == Requested || r.Phase == Starting {
				pending++
			}
			if r.holdsJob() {
				held[r.ScaleSet]++
			}
		}
		room := s.Classes[class].Max - active
		if backoff.holding() {
			// After a hold, one runner at a time tries whether the class works again.
			room = min(room, 1-pending)
		}
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
		// A warm repository's next runner waits behind every job, so warm
		// runners never take the place a waiting job needs.
		for _, key := range slices.SortedFunc(maps.Keys(s.ScaleSets), func(a, b Key) int { return cmp.Compare(a.Repo, b.Repo) }) {
			if key.Class == class && s.Classes[class].warm(key.Repo) && held[key] <= len(s.ScaleSets[key].Jobs) {
				queue = append(queue, waiting{key: key})
			}
		}
		for _, w := range queue[:min(room, len(queue))] {
			actions = append(actions, Start{ScaleSet: w.key})
		}
	}
	return actions
}

// Warm returns the runners kept ready for warm repositories. Runners of one
// scale set are interchangeable, so while a warm repository's scale set has
// more runners than jobs, its newest runner without a job is the warm one.
func Warm(s State) map[string]bool {
	warm := map[string]bool{}
	for key, ss := range s.ScaleSets {
		if !s.Classes[key.Class].warm(key.Repo) {
			continue
		}
		held := 0
		var newest *Runner
		for _, r := range s.Runners {
			if r.ScaleSet != key || !r.holdsJob() {
				continue
			}
			held++
			if r.Phase != Running && (newest == nil || cmp.Or(r.Since.Compare(newest.Since), cmp.Compare(r.Name, newest.Name)) > 0) {
				newest = &r
			}
		}
		if newest != nil && held > len(ss.Jobs) {
			warm[newest.Name] = true
		}
	}
	return warm
}

// normalBusy reports whether a normal-priority job runs or is about to.
func normalBusy(s State, warm map[string]bool) bool {
	for _, r := range s.Runners {
		if c, ok := s.Classes[r.ScaleSet.Class]; ok && c.Priority == config.Normal && r.holdsJob() && !warm[r.Name] {
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

// ScaleSetGone means the agent stopped listening to a scale set, with HeldBy
// set when another box's session holds it.
type ScaleSetGone struct {
	ScaleSet Key
	HeldBy   string
}

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

// StartFailed means a runner did not start. Container is set when its
// container failed to be created or started, which counts toward a Backoff.
type StartFailed struct {
	Runner    string
	Container bool
}

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
	warm := Warm(s)
	s.Runners = maps.Clone(s.Runners)
	s.ScaleSets = maps.Clone(s.ScaleSets)
	s.HeldBy = maps.Clone(s.HeldBy)
	s.Backoff = maps.Clone(s.Backoff)
	if s.Backoff == nil {
		s.Backoff = map[string]Backoff{}
	}
	update := func(name string, f func(*Runner)) {
		if r, ok := s.Runners[name]; ok {
			f(&r)
			s.Runners[name] = r
			if r.TookJob {
				delete(s.Backoff, r.ScaleSet.Class)
			}
		}
	}
	failed := func(r Runner) {
		b := s.Backoff[r.ScaleSet.Class]
		b.Failures++
		if b.holding() {
			b.Until, b.Runner = now.Add(hold(b.Failures)), r.Name
		}
		s.Backoff[r.ScaleSet.Class] = b
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
				r.TookJob, r.Result = true, j.Result
				if keep := s.Classes[r.ScaleSet.Class].KeepFailed; keep > 0 && j.Result == "failed" {
					r.KeepUntil = now.Add(keep)
				}
			})
		}
		s.ScaleSets[e.ScaleSet] = ScaleSet{ID: e.ID, Jobs: e.Jobs}
		delete(s.HeldBy, e.ScaleSet)
	case ScaleSetGone:
		delete(s.ScaleSets, e.ScaleSet)
		delete(s.HeldBy, e.ScaleSet)
		if e.HeldBy != "" {
			s.HeldBy[e.ScaleSet] = e.HeldBy
		}
	case Observed:
		s.Observed = true
		s.DiskFree = e.DiskFree
		before := maps.Clone(s.Runners)
		s.Runners = observe(s.Runners, e.Containers, now)
		for _, name := range slices.Sorted(maps.Keys(s.Runners)) {
			if r := s.Runners[name]; before[name].Phase == Starting && r.Phase == Exited && !r.TookJob {
				failed(r)
			}
		}
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
		if r, ok := s.Runners[e.Runner]; ok && e.Container {
			failed(r)
		}
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
	// A warm runner's wait does not count: once a job arrives for it, it has
	// the idle timeout from then to take the job.
	still := Warm(s)
	for name := range warm {
		if r, ok := s.Runners[name]; ok && r.Phase == Starting && !still[name] {
			r.Since = now
			s.Runners[name] = r
		}
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

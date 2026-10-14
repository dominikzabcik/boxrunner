package scheduler

import (
	"fmt"
	"maps"
	"reflect"
	"testing"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/config"
	"github.com/dominikzabcik/boxrunner/internal/queue"
)

var t0 = time.Date(2026, 10, 13, 21, 0, 0, 0, time.UTC)

func at(minutes float64) time.Time { return t0.Add(time.Duration(minutes * float64(time.Minute))) }

var (
	siteMain  = Key{"octo/site", "boxrunner"}
	apiMain   = Key{"octo/api", "boxrunner"}
	siteLight = Key{"octo/site", "boxrunner-light"}
)

const gb = 1_000_000_000

func state(maxMain, maxLight int, sets map[Key]ScaleSet, runners ...Runner) State {
	s := State{
		Classes: map[string]Class{
			"boxrunner":       {Max: maxMain, Priority: config.Normal},
			"boxrunner-light": {Max: maxLight, Priority: config.Idle},
		},
		ScaleSets: sets,
		HeldBy:    map[Key]string{},
		Runners:   map[string]Runner{},
		DiskFloor: 10 * gb,
		DiskFree:  50 * gb,
		Observed:  true,
	}
	for _, r := range runners {
		s.Runners[r.Name] = r
	}
	return s
}

// warm keeps one idle runner ready for each repository in the main class.
func warm(s State, repos ...string) State {
	c := s.Classes["boxrunner"]
	c.Warm = repos
	s.Classes = maps.Clone(s.Classes)
	s.Classes["boxrunner"] = c
	return s
}

func jobs(minutes ...float64) []time.Time {
	var out []time.Time
	for _, m := range minutes {
		out = append(out, at(m))
	}
	return out
}

func TestDecide(t *testing.T) {
	now := at(10)
	cases := []struct {
		name  string
		state State
		want  []Action
	}{
		{
			name: "nothing before the first look at Docker",
			state: func() State {
				s := state(1, 2, map[Key]ScaleSet{siteMain: {ID: 7, Jobs: jobs(1)}})
				s.Observed = false
				return s
			}(),
			want: nil,
		},
		{
			name:  "no jobs, no runners",
			state: state(1, 2, map[Key]ScaleSet{siteMain: {ID: 7}}),
			want:  nil,
		},
		{
			name:  "the oldest waiting job across repositories goes first",
			state: state(1, 2, map[Key]ScaleSet{siteMain: {ID: 7, Jobs: jobs(5)}, apiMain: {ID: 9, Jobs: jobs(1)}}),
			want:  []Action{Start{apiMain}},
		},
		{
			name: "runners take a scale set's oldest jobs, so its newer ones wait",
			state: state(2, 2,
				map[Key]ScaleSet{siteMain: {ID: 7, Jobs: jobs(1, 4)}, apiMain: {ID: 9, Jobs: jobs(3)}},
				Runner{Name: "r1", ScaleSet: siteMain, Phase: Running, TookJob: true}),
			want: []Action{Start{apiMain}},
		},
		{
			name:  "several runners for one scale set up to max",
			state: state(2, 2, map[Key]ScaleSet{siteMain: {ID: 7, Jobs: jobs(1, 2, 3)}}),
			want:  []Action{Start{siteMain}, Start{siteMain}},
		},
		{
			name: "a full class starts nothing, another class still starts",
			state: state(1, 2,
				map[Key]ScaleSet{siteMain: {ID: 7, Jobs: jobs(1, 2)}, apiMain: {ID: 9, Jobs: jobs(0)}, siteLight: {ID: 8, Jobs: jobs(3)}},
				Runner{Name: "r1", ScaleSet: siteMain, Phase: Requested, Since: at(9)}),
			want: []Action{Start{siteLight}},
		},
		{
			name: "a runner being removed still counts against max",
			state: state(1, 2, map[Key]ScaleSet{siteMain: {ID: 7, Jobs: jobs(1)}},
				Runner{Name: "r1", ScaleSet: apiMain, Phase: Removing}),
			want: nil,
		},
		{
			name: "an exited runner is removed and frees its place",
			state: state(1, 2, map[Key]ScaleSet{siteMain: {ID: 7, Jobs: jobs(1)}},
				Runner{Name: "r1", ScaleSet: siteMain, Phase: Exited, TookJob: true}),
			want: []Action{Remove{Runner: "r1", Reason: "exited"}, Start{siteMain}},
		},
		{
			name: "an exited runner that never took a job is also removed at GitHub",
			state: state(1, 2, map[Key]ScaleSet{},
				Runner{Name: "r1", ScaleSet: siteMain, Phase: Exited}),
			want: []Action{Remove{Runner: "r1", Deregister: true, Reason: "exited"}},
		},
		{
			name: "a runner without a job for three minutes is removed",
			state: state(2, 2, map[Key]ScaleSet{siteMain: {ID: 7, Jobs: jobs(1)}},
				Runner{Name: "r1", ScaleSet: siteMain, Phase: Starting, Since: at(7)},
				Runner{Name: "r2", ScaleSet: apiMain, Phase: Starting, Since: at(7.01)}),
			want: []Action{Remove{Runner: "r1", Deregister: true, Reason: "took no job within 3 minutes"}},
		},
		{
			name: "below the disk floor nothing starts",
			state: func() State {
				s := state(1, 2, map[Key]ScaleSet{siteMain: {ID: 7, Jobs: jobs(1)}})
				s.DiskFree = 9 * gb
				return s
			}(),
			want: nil,
		},
		{
			name: "below half the floor the largest job container stops",
			state: func() State {
				s := state(1, 2, map[Key]ScaleSet{siteLight: {ID: 8, Jobs: jobs(1, 2)}},
					Runner{Name: "r1", ScaleSet: siteLight, Phase: Running, TookJob: true, Disk: 3 * gb},
					Runner{Name: "r2", ScaleSet: siteLight, Phase: Running, TookJob: true, Disk: 8 * gb},
					Runner{Name: "r3", ScaleSet: siteMain, Phase: Starting, Since: at(9), Disk: 9 * gb})
				s.DiskFree = 4 * gb
				return s
			}(),
			want: []Action{Remove{Runner: "r2", Reason: "free disk below half the floor"}},
		},
		{
			name: "one disk removal at a time",
			state: func() State {
				s := state(1, 2, map[Key]ScaleSet{},
					Runner{Name: "r1", ScaleSet: siteLight, Phase: Running, TookJob: true, Disk: 3 * gb},
					Runner{Name: "r2", ScaleSet: siteLight, Phase: Removing, TookJob: true, Disk: 8 * gb})
				s.DiskFree = 4 * gb
				return s
			}(),
			want: nil,
		},
		{
			name: "a new runner version builds while only idle jobs run",
			state: func() State {
				s := state(1, 2, map[Key]ScaleSet{}, Runner{Name: "r1", ScaleSet: siteLight, Phase: Running, TookJob: true})
				s.Image = Image{Built: "2.328.0", Latest: "2.329.0", LatestDigest: "sha256:4ffa"}
				return s
			}(),
			want: []Action{Build{Version: "2.329.0", Digest: "sha256:4ffa"}},
		},
		{
			name: "never while a normal-priority job runs",
			state: func() State {
				s := state(1, 2, map[Key]ScaleSet{}, Runner{Name: "r1", ScaleSet: siteMain, Phase: Running, TookJob: true})
				s.Image = Image{Built: "2.328.0", Latest: "2.329.0"}
				return s
			}(),
			want: nil,
		},
		{
			name:  "a warm repository gets an idle runner",
			state: warm(state(2, 2, map[Key]ScaleSet{siteMain: {ID: 7}, apiMain: {ID: 9}}), "octo/site"),
			want:  []Action{Start{siteMain}},
		},
		{
			name:  "only once its scale set is listened to",
			state: warm(state(2, 2, map[Key]ScaleSet{apiMain: {ID: 9}}), "octo/site"),
			want:  nil,
		},
		{
			name: "the warm runner is never removed for idleness; a second idle one is",
			state: warm(state(3, 2, map[Key]ScaleSet{siteMain: {ID: 7}},
				Runner{Name: "r1", ScaleSet: siteMain, Phase: Starting, Since: at(0)},
				Runner{Name: "r2", ScaleSet: siteMain, Phase: Starting, Since: at(1)}), "octo/site"),
			want: []Action{Remove{Runner: "r1", Deregister: true, Reason: "took no job within 3 minutes"}},
		},
		{
			name: "the warm runner counts toward max",
			state: warm(state(2, 2, map[Key]ScaleSet{siteMain: {ID: 7}, apiMain: {ID: 9, Jobs: jobs(1, 2)}},
				Runner{Name: "r1", ScaleSet: siteMain, Phase: Starting, Since: at(0)}), "octo/site"),
			want: []Action{Start{apiMain}},
		},
		{
			name: "no refill while another repository's job waits for the place",
			state: warm(state(2, 2, map[Key]ScaleSet{siteMain: {ID: 7, Jobs: jobs(1)}, apiMain: {ID: 9, Jobs: jobs(5)}},
				Runner{Name: "r1", ScaleSet: siteMain, Phase: Running, TookJob: true}), "octo/site"),
			want: []Action{Start{apiMain}},
		},
		{
			name: "nor is a warm runner evicted for it",
			state: warm(state(2, 2, map[Key]ScaleSet{siteMain: {ID: 7}, apiMain: {ID: 9, Jobs: jobs(1, 5)}},
				Runner{Name: "r1", ScaleSet: siteMain, Phase: Starting, Since: at(0)},
				Runner{Name: "r2", ScaleSet: apiMain, Phase: Running, TookJob: true}), "octo/site"),
			want: nil,
		},
		{
			name: "a warm runner does not hold back an image build",
			state: func() State {
				s := warm(state(1, 2, map[Key]ScaleSet{siteMain: {ID: 7}},
					Runner{Name: "r1", ScaleSet: siteMain, Phase: Starting, Since: at(0)}), "octo/site")
				s.Image = Image{Built: "2.328.0", Latest: "2.329.0", LatestDigest: "sha256:4ffa"}
				return s
			}(),
			want: []Action{Build{Version: "2.329.0", Digest: "sha256:4ffa"}},
		},
		{
			name: "not while building or before the retry time",
			state: func() State {
				s := state(1, 2, map[Key]ScaleSet{})
				s.Image = Image{Built: "2.328.0", Latest: "2.329.0", RetryAt: at(10.5)}
				return s
			}(),
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Decide(c.state, now); !reflect.DeepEqual(got, c.want) {
				t.Errorf("Decide =\n%#v\nwant\n%#v", got, c.want)
			}
		})
	}
}

func TestApply(t *testing.T) {
	cases := []struct {
		name   string
		before State
		events []Event
		want   map[string]Runner
	}{
		{
			name:   "a start goes requested, starting, running, exited",
			before: state(1, 2, map[Key]ScaleSet{siteMain: {ID: 7, Jobs: jobs(1)}}),
			events: []Event{
				StartDispatched{"r1", siteMain}, Started{"r1", 41},
				Update{ScaleSet: siteMain, ID: 7, Jobs: jobs(1), Started: []string{"r1"}},
				Update{ScaleSet: siteMain, ID: 7, Completed: []queue.JobCompleted{{RunnerName: "r1"}}},
			},
			want: map[string]Runner{"r1": {Name: "r1", ScaleSet: siteMain, ScaleSetID: 7, ID: 41, Phase: Exited, Since: at(10), TookJob: true}},
		},
		{
			name:   "a job that starts before the container is reported stays running",
			before: state(1, 2, map[Key]ScaleSet{siteMain: {ID: 7, Jobs: jobs(1)}}),
			events: []Event{StartDispatched{"r1", siteMain}, Update{ScaleSet: siteMain, ID: 7, Jobs: jobs(1), Started: []string{"r1"}}, Started{"r1", 41}},
			want:   map[string]Runner{"r1": {Name: "r1", ScaleSet: siteMain, ScaleSetID: 7, ID: 41, Phase: Running, Since: at(10), TookJob: true}},
		},
		{
			name:   "a failed start and a finished removal forget the runner",
			before: state(1, 2, map[Key]ScaleSet{}, Runner{Name: "r1", Phase: Requested}, Runner{Name: "r2", Phase: Removing}),
			events: []Event{StartFailed{Runner: "r1"}, Removed{"r2"}},
			want:   map[string]Runner{},
		},
		{
			name:   "GitHub refusing a removal marks the runner running",
			before: state(1, 2, map[Key]ScaleSet{}, Runner{Name: "r1", Phase: Starting, Since: at(1)}),
			events: []Event{RemoveDispatched{"r1"}, Busy{"r1"}},
			want:   map[string]Runner{"r1": {Name: "r1", Phase: Running, Since: at(10), TookJob: true}},
		},
		{
			name: "after a restart, labelled containers are adopted from their creation time",
			before: func() State {
				s := state(1, 2, map[Key]ScaleSet{})
				s.Observed = false
				return s
			}(),
			events: []Event{Observed{DiskFree: 40 * gb, Containers: []Container{
				{Runner: "r1", ScaleSet: siteMain, ScaleSetID: 7, RunnerID: 41, Running: true, Created: at(2), Disk: 5},
				{Runner: "r2", ScaleSet: siteLight, ScaleSetID: 8, RunnerID: 42, Running: false, Created: at(3)},
			}}},
			want: map[string]Runner{
				"r1": {Name: "r1", ScaleSet: siteMain, ScaleSetID: 7, ID: 41, Phase: Starting, Since: at(2), Disk: 5},
				"r2": {Name: "r2", ScaleSet: siteLight, ScaleSetID: 8, ID: 42, Phase: Exited, Since: at(3)},
			},
		},
		{
			name: "a container that stopped or vanished means its runner exited; one still being created is left alone",
			before: state(1, 2, map[Key]ScaleSet{},
				Runner{Name: "r1", Phase: Running, TookJob: true, Since: at(1)},
				Runner{Name: "r2", Phase: Starting, Since: at(1)},
				Runner{Name: "r3", Phase: Requested, Since: at(9)},
				Runner{Name: "r4", Phase: Removing, Since: at(9)}),
			events: []Event{Observed{DiskFree: 40 * gb, Containers: []Container{{Runner: "r1", Running: false, Disk: 7}}}},
			want: map[string]Runner{
				"r1": {Name: "r1", Phase: Exited, TookJob: true, Since: at(10), Disk: 7},
				"r2": {Name: "r2", Phase: Exited, Since: at(10)},
				"r3": {Name: "r3", Phase: Requested, Since: at(9)},
				"r4": {Name: "r4", Phase: Removing, Since: at(9)},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := c.before
			for _, e := range c.events {
				s = Apply(s, e, at(10))
			}
			if !reflect.DeepEqual(s.Runners, c.want) {
				t.Errorf("runners =\n%+v\nwant\n%+v", s.Runners, c.want)
			}
		})
	}
}

// Adoption and the idle rule together: after a restart, a container whose
// runner never took a job is probed and removed; a busy one is kept.
func TestRestartConverges(t *testing.T) {
	s := state(1, 2, map[Key]ScaleSet{})
	s.Observed = false
	s = Apply(s, Observed{DiskFree: 40 * gb, Containers: []Container{
		{Runner: "old", ScaleSet: siteMain, Running: true, Created: at(0)},
		{Runner: "busy", ScaleSet: siteLight, Running: true, Created: at(0)},
	}}, at(10))
	want := []Action{
		Remove{Runner: "busy", Deregister: true, Reason: "took no job within 3 minutes"},
		Remove{Runner: "old", Deregister: true, Reason: "took no job within 3 minutes"},
	}
	if got := Decide(s, at(10)); !reflect.DeepEqual(got, want) {
		t.Fatalf("Decide = %#v", got)
	}
	for _, e := range []Event{RemoveDispatched{"busy"}, RemoveDispatched{"old"}, Busy{"busy"}, Removed{"old"}} {
		s = Apply(s, e, at(10))
	}
	if got := Decide(s, at(20)); got != nil {
		t.Errorf("Decide after convergence = %#v", got)
	}
	if r := s.Runners["busy"]; len(s.Runners) != 1 || r.Phase != Running {
		t.Errorf("runners = %+v", s.Runners)
	}
}

// A job completing and its scale set's count going down arrive in one message;
// between the two the scheduler must not see a job without a runner.
func TestFinishedJobStartsNothing(t *testing.T) {
	s := state(1, 2, map[Key]ScaleSet{siteMain: {ID: 7, Jobs: jobs(1)}},
		Runner{Name: "r1", ScaleSet: siteMain, ScaleSetID: 7, Phase: Running, TookJob: true})
	s = Apply(s, Update{ScaleSet: siteMain, ID: 7, Completed: []queue.JobCompleted{{RunnerName: "r1"}}}, at(10))
	want := []Action{Remove{Runner: "r1", Reason: "exited"}}
	if got := Decide(s, at(10)); !reflect.DeepEqual(got, want) {
		t.Errorf("Decide = %#v", got)
	}
}

// A job arrives for the warm runner, which waited ten minutes: a new warm
// runner starts beside it, the first gets three minutes to take the job, and
// when the job ends nothing more starts.
func TestWarmRunnerIsReplaced(t *testing.T) {
	s := warm(state(2, 2, map[Key]ScaleSet{siteMain: {ID: 7}},
		Runner{Name: "w1", ScaleSet: siteMain, ScaleSetID: 7, Phase: Starting, Since: at(0)}), "octo/site")
	if got := Warm(s); !reflect.DeepEqual(got, map[string]bool{"w1": true}) {
		t.Errorf("Warm before the job = %v", got)
	}
	s = Apply(s, Update{ScaleSet: siteMain, ID: 7, Jobs: jobs(10)}, at(10))
	if got, want := Decide(s, at(10)), []Action{Start{siteMain}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Decide when a job arrives = %#v", got)
	}
	s = Apply(s, StartDispatched{"w2", siteMain}, at(10))
	s = Apply(s, Started{"w2", 42}, at(10.1))
	if got := Warm(s); !reflect.DeepEqual(got, map[string]bool{"w2": true}) {
		t.Errorf("Warm once the job arrived = %v", got)
	}
	if got, want := Decide(s, at(13.5)), []Action{Remove{Runner: "w1", Deregister: true, Reason: "took no job within 3 minutes"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("Decide if the job is not taken in 3 minutes = %#v", got)
	}
	s = Apply(s, Update{ScaleSet: siteMain, ID: 7, Jobs: jobs(10), Started: []string{"w1"}}, at(10.2))
	if got := Decide(s, at(13.5)); got != nil {
		t.Errorf("Decide while the job runs = %#v", got)
	}
	s = Apply(s, Update{ScaleSet: siteMain, ID: 7, Completed: []queue.JobCompleted{{RunnerName: "w1"}}}, at(20))
	if got, want := Decide(s, at(20)), []Action{Remove{Runner: "w1", Reason: "exited"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("Decide after the job = %#v", got)
	}
	if got := Decide(Apply(s, Removed{"w1"}, at(20)), at(60)); got != nil {
		t.Errorf("an hour later the warm runner is still kept; Decide = %#v", got)
	}
}

func TestApplyLeavesItsInputAlone(t *testing.T) {
	before := state(1, 2, map[Key]ScaleSet{siteMain: {ID: 7}}, Runner{Name: "r1", Phase: Starting})
	after := Apply(before, Removed{"r1"}, at(10))
	after = Apply(after, ScaleSetGone{ScaleSet: siteMain}, at(10))
	if len(before.Runners) != 1 || len(before.ScaleSets) != 1 || len(after.Runners) != 0 || len(after.ScaleSets) != 0 {
		t.Errorf("before %+v, after %+v", before, after)
	}
}

// Another box's session names the scale set until this box's own opens.
func TestHeldBy(t *testing.T) {
	held := Apply(state(1, 2, map[Key]ScaleSet{}), ScaleSetGone{ScaleSet: siteMain, HeldBy: "box-2"}, at(1))
	if want := map[Key]string{siteMain: "box-2"}; !reflect.DeepEqual(held.HeldBy, want) {
		t.Errorf("HeldBy = %v; want %v", held.HeldBy, want)
	}
	opened := Apply(held, Update{ScaleSet: siteMain, ID: 7}, at(2))
	if len(opened.HeldBy) != 0 || held.HeldBy[siteMain] != "box-2" {
		t.Errorf("after the session opened HeldBy = %v, before it %v", opened.HeldBy, held.HeldBy)
	}
}

func TestImageEvents(t *testing.T) {
	s := state(1, 2, map[Key]ScaleSet{})
	s = Apply(s, LatestRunner{Version: "2.329.0", Digest: "sha256:4ffa"}, at(0))
	s = Apply(s, BuildDispatched{}, at(0))
	s = Apply(s, BuildFailed{}, at(1))
	if want := (Image{Latest: "2.329.0", LatestDigest: "sha256:4ffa", RetryAt: at(61)}); s.Image != want {
		t.Errorf("after a failed build: %+v", s.Image)
	}
	if got := Decide(s, at(61)); !reflect.DeepEqual(got, []Action{Build{Version: "2.329.0", Digest: "sha256:4ffa"}}) {
		t.Errorf("Decide an hour later = %#v", got)
	}
	s = Apply(s, ImageBuilt{Version: "2.329.0", Digest: "sha256:4ffa"}, at(62))
	if want := (Image{Built: "2.329.0", Digest: "sha256:4ffa", Latest: "2.329.0", LatestDigest: "sha256:4ffa", RetryAt: at(61)}); s.Image != want {
		t.Errorf("after a build: %+v", s.Image)
	}
}

// While the CI has not verified the runner's latest release, the agent builds
// nothing newer than the release it did verify.
func TestUnlistedRunner(t *testing.T) {
	s := state(1, 2, map[Key]ScaleSet{})
	s = Apply(s, ImageBuilt{Version: "2.329.0", Digest: "sha256:4ffa"}, at(0))
	s = Apply(s, LatestRunner{Version: "2.329.0", Digest: "sha256:4ffa", Unlisted: "2.330.0"}, at(0))
	if got := Decide(s, at(1)); got != nil {
		t.Errorf("Decide with 2.330.0 unlisted = %#v", got)
	}
	s = Apply(s, LatestRunner{Version: "2.330.0", Digest: "sha256:e549"}, at(60))
	if got := Decide(s, at(60)); !reflect.DeepEqual(got, []Action{Build{Version: "2.330.0", Digest: "sha256:e549"}}) {
		t.Errorf("Decide once 2.330.0 is listed = %#v", got)
	}
	if s.Image.Unlisted != "" {
		t.Errorf("Unlisted = %q once 2.330.0 is listed", s.Image.Unlisted)
	}
}

// A refusal stays until a build succeeds, so a registry that is down at the
// next attempt does not hide it.
func TestRefusedImage(t *testing.T) {
	refused := "ghcr.io/actions/actions-runner:2.330.0 points to sha256:0bad, not to sha256:e549 that boxrunner's CI verified"
	s := state(1, 2, map[Key]ScaleSet{})
	s = Apply(s, ImageBuilt{Version: "2.329.0", Digest: "sha256:4ffa"}, at(0))
	s = Apply(s, LatestRunner{Version: "2.330.0", Digest: "sha256:e549"}, at(0))
	s = Apply(s, BuildDispatched{}, at(0))
	s = Apply(s, BuildFailed{Refused: refused}, at(1))
	s = Apply(s, BuildDispatched{}, at(61))
	s = Apply(s, BuildFailed{}, at(62))
	want := Image{Built: "2.329.0", Digest: "sha256:4ffa", Latest: "2.330.0", LatestDigest: "sha256:e549", RetryAt: at(122), Refused: refused}
	if s.Image != want {
		t.Errorf("after a refusal and a failed attempt: %+v", s.Image)
	}
	s = Apply(s, ImageBuilt{Version: "2.330.0", Digest: "sha256:e549"}, at(123))
	if want := (Image{Built: "2.330.0", Digest: "sha256:e549", Latest: "2.330.0", LatestDigest: "sha256:e549", RetryAt: at(122)}); s.Image != want {
		t.Errorf("after a build: %+v", s.Image)
	}
}

func TestKeepFailed(t *testing.T) {
	keeping := func(runners ...Runner) State {
		s := state(1, 2, map[Key]ScaleSet{}, runners...)
		c := s.Classes["boxrunner"]
		c.KeepFailed = 15 * time.Minute
		s.Classes["boxrunner"] = c
		return s
	}
	failed := Update{ScaleSet: siteMain, ID: 7, Completed: []queue.JobCompleted{{Result: "failed", RunnerName: "r1"}}}
	ok := Update{ScaleSet: siteMain, ID: 7, Completed: []queue.JobCompleted{{Result: "succeeded", RunnerName: "r1"}}}
	running := Runner{Name: "r1", ScaleSet: siteMain, ScaleSetID: 7, ID: 41, Phase: Running, Since: at(0), TookJob: true}

	s := Apply(keeping(running), failed, at(5))
	if got := Decide(s, at(19)); len(got) != 0 {
		t.Errorf("a failed job is kept for 15 minutes; at 14 min Decide = %v", got)
	}
	if got, want := Decide(s, at(20)), []Action{Remove{Runner: "r1", Reason: "exited"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("after 15 min Decide = %v, want %v", got, want)
	}
	if got, want := Decide(Apply(keeping(running), ok, at(5)), at(5)), []Action{Remove{Runner: "r1", Reason: "exited"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("a succeeded job is removed at once; Decide = %v, want %v", got, want)
	}

	// The container stops before GitHub says how the job ended.
	stopped := Apply(keeping(running), Observed{DiskFree: 50 * gb}, at(5))
	if got := Decide(stopped, at(5.25)); len(got) != 0 {
		t.Errorf("a stopped container waits for its result; Decide = %v", got)
	}
	if got := Decide(Apply(stopped, failed, at(5.4)), at(6)); len(got) != 0 {
		t.Errorf("then a failed result keeps it; Decide = %v", got)
	}
	if got, want := Decide(stopped, at(6)), []Action{Remove{Runner: "r1", Reason: "exited"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("without a result for 30 s it is removed; Decide = %v, want %v", got, want)
	}

	// A class that keeps nothing removes a failed job at once.
	plain := Apply(state(1, 2, map[Key]ScaleSet{}, running), failed, at(5))
	if got, want := Decide(plain, at(5)), []Action{Remove{Runner: "r1", Reason: "exited"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("without keep_failed Decide = %v, want %v", got, want)
	}
}

// drive plays the agent for a while: it carries out every decision at once,
// and every runner's container exits five seconds after it starts, before it
// takes a job. It returns the seconds at which runners started and the
// runners whose logs were asked for.
func drive(s State, until time.Time) (started []float64, logged []string) {
	n := 0
	for now := at(0); now.Before(until); now = now.Add(5 * time.Second) {
		s = Apply(s, Observed{DiskFree: 50 * gb}, now)
		for _, a := range Decide(s, now) {
			switch a := a.(type) {
			case Start:
				n++
				name := fmt.Sprintf("r%d", n)
				s = Apply(Apply(s, StartDispatched{name, a.ScaleSet}, now), Started{name, int64(n)}, now)
				started = append(started, now.Sub(t0).Seconds())
			case Remove:
				if a.Logs {
					logged = append(logged, a.Runner)
				}
				s = Apply(Apply(s, RemoveDispatched{a.Runner}, now), Removed{a.Runner}, now)
			}
		}
	}
	return started, logged
}

// Without the hold, a class whose containers exit at once starts a runner on
// every decision: 1,440 in two hours here. With it, the class tries three
// times, then after 1, 2, 4, 8, 16 and 30 minutes.
func TestCrashLoopHolds(t *testing.T) {
	started, logged := drive(state(1, 2, map[Key]ScaleSet{siteMain: {ID: 7, Jobs: jobs(0)}}), at(120))
	if want := []float64{0, 5, 10, 75, 200, 445, 930, 1895, 3700, 5505}; !reflect.DeepEqual(started, want) {
		t.Errorf("runners started at %v s, want %v", started, want)
	}
	if want := []string{"r3", "r4", "r5", "r6", "r7", "r8", "r9", "r10"}; !reflect.DeepEqual(logged, want) {
		t.Errorf("logs asked for %v, want %v", logged, want)
	}
}

// After a hold, a class with room for two jobs tries one runner at a time.
func TestCrashLoopTriesOneRunner(t *testing.T) {
	s := state(1, 2, map[Key]ScaleSet{siteLight: {ID: 8, Jobs: jobs(0, 1)}})
	s.Backoff = map[string]Backoff{"boxrunner-light": {Failures: 3, Until: at(10), Runner: "r3"}}
	if got := Decide(s, at(9)); got != nil {
		t.Errorf("Decide while held = %#v", got)
	}
	if got, want := Decide(s, at(10)), []Action{Start{siteLight}}; !reflect.DeepEqual(got, want) {
		t.Errorf("Decide after the hold = %#v, want %#v", got, want)
	}
	s = Apply(s, StartDispatched{"r4", siteLight}, at(10))
	if got := Decide(s, at(10)); got != nil {
		t.Errorf("Decide while the one runner waits = %#v", got)
	}
	s = Apply(s, Update{ScaleSet: siteLight, ID: 8, Jobs: jobs(0, 1), Started: []string{"r4"}}, at(10.5))
	if got, want := Decide(s, at(10.5)), []Action{Start{siteLight}}; !reflect.DeepEqual(got, want) || len(s.Backoff) != 0 {
		t.Errorf("once it takes a job: Decide = %#v, backoff %v", got, s.Backoff)
	}
}

func TestBackoffCounts(t *testing.T) {
	starting := func(name string) Runner {
		return Runner{Name: name, ScaleSet: siteMain, ScaleSetID: 7, Phase: Starting, Since: at(0)}
	}
	cases := []struct {
		name   string
		before State
		events []Event
		want   map[string]Backoff
	}{
		{
			name:   "a container that exits before a job counts",
			before: state(1, 2, map[Key]ScaleSet{}, starting("r1")),
			events: []Event{Observed{DiskFree: 50 * gb}},
			want:   map[string]Backoff{"boxrunner": {Failures: 1}},
		},
		{
			name:   "the third failure holds the class for a minute",
			before: state(1, 2, map[Key]ScaleSet{}, starting("r1"), starting("r2"), starting("r3")),
			events: []Event{Observed{DiskFree: 50 * gb}},
			want:   map[string]Backoff{"boxrunner": {Failures: 3, Until: at(11), Runner: "r3"}},
		},
		{
			name:   "a container that failed to start counts; a runner config that could not be minted does not",
			before: state(1, 2, map[Key]ScaleSet{}, starting("r1"), starting("r2")),
			events: []Event{StartFailed{Runner: "r1", Container: true}, StartFailed{Runner: "r2"}},
			want:   map[string]Backoff{"boxrunner": {Failures: 1}},
		},
		{
			name:   "a runner removed for taking no job does not count",
			before: state(1, 2, map[Key]ScaleSet{}, starting("r1")),
			events: []Event{RemoveDispatched{"r1"}, Removed{"r1"}, Observed{DiskFree: 50 * gb}},
			want:   map[string]Backoff{},
		},
		{
			name: "a runner that takes a job ends the hold",
			before: func() State {
				s := state(1, 2, map[Key]ScaleSet{}, starting("r1"))
				s.Backoff = map[string]Backoff{"boxrunner": {Failures: 5, Until: at(4), Runner: "r0"}, "boxrunner-light": {Failures: 1}}
				return s
			}(),
			events: []Event{Update{ScaleSet: siteMain, ID: 7, Started: []string{"r1"}}},
			want:   map[string]Backoff{"boxrunner-light": {Failures: 1}},
		},
		{
			name: "so does a job that completes before its start is reported",
			before: func() State {
				s := state(1, 2, map[Key]ScaleSet{}, starting("r1"))
				s.Backoff = map[string]Backoff{"boxrunner": {Failures: 2}}
				return s
			}(),
			events: []Event{Observed{DiskFree: 50 * gb}, Update{ScaleSet: siteMain, ID: 7, Completed: []queue.JobCompleted{{RunnerName: "r1", Result: "succeeded"}}}},
			want:   map[string]Backoff{},
		},
		{
			name: "the hold doubles and stops at 30 minutes",
			before: func() State {
				s := state(1, 2, map[Key]ScaleSet{}, starting("r9"))
				s.Backoff = map[string]Backoff{"boxrunner": {Failures: 7, Until: at(5), Runner: "r8"}}
				return s
			}(),
			events: []Event{Observed{DiskFree: 50 * gb}},
			want:   map[string]Backoff{"boxrunner": {Failures: 8, Until: at(40), Runner: "r9"}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := c.before
			for _, e := range c.events {
				s = Apply(s, e, at(10))
			}
			if !reflect.DeepEqual(s.Backoff, c.want) {
				t.Errorf("backoff = %+v, want %+v", s.Backoff, c.want)
			}
		})
	}
}

func TestMaxDuration(t *testing.T) {
	s := state(1, 2, map[Key]ScaleSet{},
		Runner{Name: "r1", ScaleSet: siteMain, Phase: Running, TookJob: true, Since: at(0)},
		Runner{Name: "r2", ScaleSet: siteLight, Phase: Running, TookJob: true, Since: at(0)})
	c := s.Classes["boxrunner"]
	c.MaxDuration = 6 * time.Hour
	s.Classes["boxrunner"] = c
	if got := Decide(s, at(359)); got != nil {
		t.Errorf("Decide before 6 hours = %#v", got)
	}
	want := []Action{Remove{Runner: "r1", Reason: "job ran longer than max_duration (6h0m0s)"}}
	if got := Decide(s, at(360)); !reflect.DeepEqual(got, want) {
		t.Errorf("Decide after 6 hours = %#v, want %#v", got, want)
	}
}

// A draining box starts nothing and removes every runner without a job, the
// warm one included, while the running job keeps its runner.
func TestDrain(t *testing.T) {
	s := warm(state(4, 2, map[Key]ScaleSet{siteMain: {ID: 7, Jobs: jobs(1, 2, 3)}},
		Runner{Name: "r1", ScaleSet: siteMain, Phase: Running, TookJob: true, Since: at(1)},
		Runner{Name: "r2", ScaleSet: siteMain, Phase: Starting, Since: at(9)},
		Runner{Name: "r3", ScaleSet: siteMain, Phase: Requested, Since: at(9)},
		Runner{Name: "r4", ScaleSet: siteMain, Phase: Starting, Since: at(9.5)},
	), "octo/site")
	if w := Warm(s); !w["r4"] {
		t.Fatalf("Warm = %v; want r4", w)
	}
	s = Apply(s, Drain{On: true}, at(10))
	want := []Action{
		Remove{Runner: "r2", Deregister: true, Reason: "draining"},
		Remove{Runner: "r4", Deregister: true, Reason: "draining"},
	}
	if got := Decide(s, at(10)); !reflect.DeepEqual(got, want) {
		t.Errorf("Decide while draining = %#v, want %#v", got, want)
	}
}

func TestResumeStartsAgain(t *testing.T) {
	s := Apply(state(1, 2, map[Key]ScaleSet{siteMain: {ID: 7, Jobs: jobs(1)}}), Drain{On: true}, at(10))
	if got := Decide(s, at(10)); got != nil {
		t.Errorf("Decide while draining = %#v", got)
	}
	s = Apply(s, Drain{On: false}, at(10))
	want := []Action{Start{ScaleSet: siteMain}}
	if got := Decide(s, at(10)); !reflect.DeepEqual(got, want) {
		t.Errorf("Decide after resuming = %#v, want %#v", got, want)
	}
}

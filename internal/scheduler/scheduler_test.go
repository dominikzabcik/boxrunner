package scheduler

import (
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
				s.Image = Image{Built: "2.328.0", Latest: "2.329.0"}
				return s
			}(),
			want: []Action{Build{Version: "2.329.0"}},
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
			events: []Event{StartFailed{"r1"}, Removed{"r2"}},
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

func TestApplyLeavesItsInputAlone(t *testing.T) {
	before := state(1, 2, map[Key]ScaleSet{siteMain: {ID: 7}}, Runner{Name: "r1", Phase: Starting})
	after := Apply(before, Removed{"r1"}, at(10))
	after = Apply(after, ScaleSetGone{siteMain}, at(10))
	if len(before.Runners) != 1 || len(before.ScaleSets) != 1 || len(after.Runners) != 0 || len(after.ScaleSets) != 0 {
		t.Errorf("before %+v, after %+v", before, after)
	}
}

func TestImageEvents(t *testing.T) {
	s := state(1, 2, map[Key]ScaleSet{})
	s = Apply(s, LatestRunner{"2.329.0"}, at(0))
	s = Apply(s, BuildDispatched{}, at(0))
	s = Apply(s, BuildFailed{}, at(1))
	if want := (Image{Latest: "2.329.0", RetryAt: at(61)}); s.Image != want {
		t.Errorf("after a failed build: %+v", s.Image)
	}
	if got := Decide(s, at(61)); !reflect.DeepEqual(got, []Action{Build{"2.329.0"}}) {
		t.Errorf("Decide an hour later = %#v", got)
	}
	s = Apply(s, ImageBuilt{"2.329.0"}, at(62))
	if want := (Image{Built: "2.329.0", Latest: "2.329.0", RetryAt: at(61)}); s.Image != want {
		t.Errorf("after a build: %+v", s.Image)
	}
}

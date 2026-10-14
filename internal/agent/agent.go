// Package agent is the daemon behind `boxrunner run`. One goroutine owns the
// scheduler state; listeners, the Docker observer, the image checker and the
// executors only send it events.
package agent

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/config"
	"github.com/dominikzabcik/boxrunner/internal/disk"
	"github.com/dominikzabcik/boxrunner/internal/docker"
	"github.com/dominikzabcik/boxrunner/internal/firewall"
	"github.com/dominikzabcik/boxrunner/internal/layout"
	"github.com/dominikzabcik/boxrunner/internal/listener"
	"github.com/dominikzabcik/boxrunner/internal/mirror"
	"github.com/dominikzabcik/boxrunner/internal/queue"
	"github.com/dominikzabcik/boxrunner/internal/runnerimage"
	"github.com/dominikzabcik/boxrunner/internal/scheduler"
	"github.com/dominikzabcik/boxrunner/internal/status"
	"github.com/dominikzabcik/boxrunner/internal/worker"
)

const (
	reposEvery = 5 * time.Minute
	// Docker is observed every second tick.
	decideEvery = 5 * time.Second
	imageEvery  = 24 * time.Hour
	imageRetry  = time.Hour
	stopBudget  = 60 * time.Second
)

type Options struct {
	Config  config.Config
	Worker  *worker.Client
	Docker  *docker.Client
	Run     firewall.Run
	Version string
	Log     *slog.Logger

	// Tests replace these; zero means the real paths and timings.
	JobsLog        string
	Socket         string
	ReleasesURL    string
	Tick           time.Duration
	HeartbeatEvery time.Duration
	SysboxDir      string
}

type Agent struct {
	Options
	rootDir string
	// sysbox is whether Docker has Sysbox's runtime, which classes with
	// docker = true or isolation = "sysbox" need.
	sysbox bool
	// mirror is the registry mirror's URL for those jobs, "" when no class has Docker.
	mirror string
	queue  *queue.Client
	events chan scheduler.Event
	// stopped closes when the scheduler loop no longer reads events.
	stopped chan struct{}
	fatal   chan error
	work    sync.WaitGroup
	// resync asks supervise to list the repositories now, not at its next tick.
	resync chan struct{}

	// started and adoptedNone belong to the scheduler goroutine.
	started     time.Time
	adoptedNone bool
	// ranBefore is set once before any goroutine starts: the job log has
	// entries, so this box has run jobs and a start without containers is a
	// restart, not a new box taking over from a dead one.
	ranBefore bool

	mu       sync.Mutex
	snapshot status.Snapshot
}

// Run serves jobs until ctx ends. It returns an error only for a problem a
// restart cannot fix, such as a wrong box token.
func Run(ctx context.Context, o Options) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	o.JobsLog = cmp.Or(o.JobsLog, layout.JobsLogPath)
	o.Socket = cmp.Or(o.Socket, layout.Socket)
	o.ReleasesURL = cmp.Or(o.ReleasesURL, runnerimage.ReleasesLatest)
	o.Tick = cmp.Or(o.Tick, decideEvery)
	o.HeartbeatEvery = cmp.Or(o.HeartbeatEvery, heartbeatEvery)
	o.SysboxDir = cmp.Or(o.SysboxDir, layout.SysboxDir)
	a := &Agent{
		Options: o,
		queue:   &queue.Client{HTTP: &http.Client{Timeout: 90 * time.Second}, UserAgent: "boxrunner/" + o.Version},
		events:  make(chan scheduler.Event, 64),
		started: time.Now(),
		stopped: make(chan struct{}),
		fatal:   make(chan error, 1),
		resync:  make(chan struct{}, 1),
	}
	info, err := a.Docker.Info(ctx)
	if err != nil {
		return fmt.Errorf("docker: %w", err)
	}
	a.rootDir = info.RootDir
	_, a.sysbox = info.Runtimes[layout.SysboxRuntime]
	a.Config = a.Config.ResolveIsolation(a.sysbox)
	if err := a.converge(ctx); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.JobsLog), 0o755); err != nil {
		return err
	}
	a.ranBefore = hasEntries(a.JobsLog)
	a.Log.Info("boxrunner started", "box", a.Config.Box, "version", a.Version, "worker", a.Config.Worker.String())

	go func() {
		if err := status.Serve(ctx, a.Socket, a.status); err != nil {
			a.Log.Error("status socket", "err", err)
		}
	}()
	go a.observe(ctx)
	go a.heartbeat(ctx)
	go a.checkImage(ctx)
	supervised := make(chan struct{})
	go func() {
		defer close(supervised)
		if err := a.supervise(ctx); err != nil {
			a.raise(err)
		}
	}()

	err = a.schedule(ctx)
	cancel()
	<-supervised
	executors := make(chan struct{})
	go func() {
		a.work.Wait()
		close(executors)
	}()
	select {
	case <-executors:
	case <-time.After(stopBudget):
		a.Log.Warn("stopping without waiting for every runner start or removal")
	}
	return err
}

// converge re-applies what install set up and a reboot or Docker restart
// could have lost: the network, the registry mirror for classes with Docker
// and the firewall rules.
func (a *Agent) converge(ctx context.Context) error {
	if _, ok, err := a.Docker.Network(ctx, layout.Network); err != nil {
		return err
	} else if !ok {
		if err := a.Docker.CreateNetwork(ctx, docker.NetworkSpec{Name: layout.Network, Bridge: layout.Bridge}); err != nil {
			return fmt.Errorf("creating network %s: %w", layout.Network, err)
		}
	}
	rules := firewall.Rules{Bridge: layout.Bridge}
	if slices.ContainsFunc(a.Config.Classes, func(c config.Class) bool { return c.Docker }) {
		addr, err := mirror.Find(ctx, a.Docker)
		if err != nil {
			return err
		}
		// Without the mirror, a job's Docker pulls from Docker Hub itself.
		if err := mirror.Ensure(ctx, a.Docker, addr); err != nil {
			a.Log.Error("starting the registry mirror failed; jobs pull from Docker Hub directly", "err", err)
		}
		rules.Mirror, a.mirror = addr, mirror.URL(addr)
	}
	if err := firewall.Apply(ctx, a.Run, rules); err != nil {
		return fmt.Errorf("firewall: %w", err)
	}
	return nil
}

func (a *Agent) raise(err error) {
	select {
	case a.fatal <- err:
	default:
	}
}

// post sends an event to the scheduler unless it has stopped.
func (a *Agent) post(e scheduler.Event) {
	select {
	case a.events <- e:
	case <-a.stopped:
	}
}

func (a *Agent) schedule(ctx context.Context) error {
	defer close(a.stopped)
	state := scheduler.New(a.Config)
	tick := time.NewTicker(a.Tick)
	defer tick.Stop()
	logged := map[string]bool{}
	for {
		select {
		case e := <-a.events:
			if o, ok := e.(scheduler.Observed); ok && !state.Observed {
				a.adoptedNone = len(o.Containers) == 0
			}
			if u, ok := e.(scheduler.Update); ok {
				for _, j := range u.Completed {
					// A redelivered message must not log its job twice; a runner runs one job.
					if !logged[j.RunnerName] {
						logged[j.RunnerName] = true
						a.logJob(state, j)
					}
				}
			}
			state = scheduler.Apply(state, e, time.Now())
		case <-tick.C:
		case err := <-a.fatal:
			a.shutdown(state)
			return err
		case <-ctx.Done():
			a.shutdown(state)
			return nil
		}
		if len(logged) > 1000 {
			clear(logged)
		}
		for _, action := range scheduler.Decide(state, time.Now()) {
			state = a.dispatch(ctx, state, action)
		}
		a.publish(state)
	}
}

func (a *Agent) dispatch(ctx context.Context, s scheduler.State, action scheduler.Action) scheduler.State {
	now := time.Now()
	// Executors outlive a cancelled ctx long enough to clean up after themselves.
	work := context.WithoutCancel(ctx)
	switch act := action.(type) {
	case scheduler.Start:
		name := runnerName(a.Config.Box, act.ScaleSet.Class)
		s = scheduler.Apply(s, scheduler.StartDispatched{Runner: name, ScaleSet: act.ScaleSet}, now)
		r := s.Runners[name]
		a.spawn(func() { a.start(work, r) })
	case scheduler.Remove:
		r := s.Runners[act.Runner]
		a.Log.Info("removing runner", "runner", r.Name, "repo", r.ScaleSet.Repo, "reason", act.Reason)
		s = scheduler.Apply(s, scheduler.RemoveDispatched{Runner: act.Runner}, now)
		a.spawn(func() { a.remove(work, r, act.Deregister) })
	case scheduler.Build:
		s = scheduler.Apply(s, scheduler.BuildDispatched{}, now)
		a.spawn(func() { a.build(ctx, act.Version) })
	default:
		panic(fmt.Sprintf("agent: unhandled action %T", action))
	}
	return s
}

func (a *Agent) spawn(f func()) {
	a.work.Add(1)
	go func() {
		defer a.work.Done()
		f()
	}()
}

func runnerName(box, class string) string {
	b := make([]byte, 3)
	rand.Read(b)
	return box + "-" + class + "-" + hex.EncodeToString(b)
}

// Spec is the container for one runner: the class's limits, the bridge
// network, no mounts and no Docker socket. The runner config travels in the
// environment, never on a command line. A class isolated by Sysbox runs under
// sysbox-runc. A class with Docker also starts its own daemon, which pulls
// through mirror when one is given.
func Spec(box string, class config.Class, r scheduler.Runner, jitConfig, mirror string) docker.ContainerSpec {
	spec := docker.ContainerSpec{
		Name:  r.Name,
		Image: class.Image,
		Env:   []string{"ACTIONS_RUNNER_INPUT_JITCONFIG=" + jitConfig},
		Labels: map[string]string{
			layout.LabelBox:        box,
			layout.LabelRunner:     r.Name,
			layout.LabelRunnerID:   strconv.FormatInt(r.ID, 10),
			layout.LabelRepo:       r.ScaleSet.Repo,
			layout.LabelClass:      r.ScaleSet.Class,
			layout.LabelScaleSetID: strconv.FormatInt(r.ScaleSetID, 10),
		},
		NanoCPUs:  class.NanoCPUs,
		Memory:    int64(class.Memory),
		PidsLimit: class.Pids,
		ShmSize:   int64(class.Shm),
		Network:   layout.Network,
	}
	if class.Priority == config.Idle {
		spec.CgroupParent = layout.IdleSlice
	}
	if class.Isolation == config.Sysbox {
		spec.Runtime = layout.SysboxRuntime
	}
	if class.Docker {
		spec.Env = append(spec.Env, layout.EnvDocker)
		if mirror != "" {
			spec.Env = append(spec.Env, layout.EnvMirror+"="+mirror)
		}
	}
	return spec
}

func (a *Agent) start(ctx context.Context, r scheduler.Runner) {
	ctx, cancel := context.WithTimeout(ctx, scheduler.IdleTimeout)
	defer cancel()
	log := a.Log.With("runner", r.Name, "repo", r.ScaleSet.Repo)
	class, _ := a.Config.Class(r.ScaleSet.Class)
	minted, err := a.Worker.MintRunner(ctx, r.ScaleSet.Repo, r.ScaleSetID, r.Name)
	if err != nil {
		log.Error("minting a runner config failed", "err", err)
		if worker.Fatal(err) {
			a.raise(err)
		}
		a.post(scheduler.StartFailed{Runner: r.Name})
		return
	}
	r.ID = minted.ID
	id, err := a.Docker.Create(ctx, Spec(a.Config.Box, class, r, minted.JITConfig, a.mirror))
	if err == nil {
		err = a.Docker.Start(ctx, id)
	}
	if err != nil {
		log.Error("starting the container failed", "err", err)
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		a.Docker.Remove(cleanup, r.Name)
		if err := a.Worker.RemoveRunner(cleanup, r.ScaleSet.Repo, r.ScaleSetID, r.ID); err != nil {
			log.Warn("removing the unstarted runner failed", "err", err)
		}
		a.post(scheduler.StartFailed{Runner: r.Name})
		return
	}
	log.Info("runner started", "class", class.Name)
	a.post(scheduler.Started{Runner: r.Name, ID: r.ID})
}

func (a *Agent) remove(ctx context.Context, r scheduler.Runner, deregister bool) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if deregister && r.ID != 0 {
		err := a.Worker.RemoveRunner(ctx, r.ScaleSet.Repo, r.ScaleSetID, r.ID)
		if worker.Is(err, worker.RunnerBusy) {
			a.Log.Info("runner took a job after all; leaving it", "runner", r.Name)
			a.post(scheduler.Busy{Runner: r.Name})
			return
		}
		if err != nil {
			a.Log.Warn("removing the runner at GitHub failed", "runner", r.Name, "err", err)
		}
	}
	if r.Phase == scheduler.Running {
		// SIGTERM makes the runner cancel its job and tell GitHub; a killed
		// runner leaves the job "in progress" until GitHub gives up on it.
		if err := a.Docker.Stop(ctx, r.Name, stopGrace); err != nil {
			a.Log.Warn("stopping the container failed", "runner", r.Name, "err", err)
		}
	}
	if err := a.Docker.Remove(ctx, r.Name); err != nil {
		a.Log.Warn("removing the container failed", "runner", r.Name, "err", err)
	}
	a.post(scheduler.Removed{Runner: r.Name})
}

const stopGrace = 30 * time.Second

func (a *Agent) build(ctx context.Context, version string) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	a.Log.Info("building the job image", "runner", version)
	if err := runnerimage.Build(ctx, a.Docker, version); err != nil {
		a.Log.Error("building the job image failed", "runner", version, "err", err)
		a.post(scheduler.BuildFailed{})
		return
	}
	a.ensureClasses(ctx)
	a.post(scheduler.ImageBuilt{Version: version})
}

// ensureClasses rebuilds the class images whose job image or files changed.
func (a *Agent) ensureClasses(ctx context.Context) {
	built, err := runnerimage.EnsureClasses(ctx, a.Docker, a.Config.Classes)
	for _, class := range built {
		a.Log.Info("built the class image", "class", class)
	}
	if err != nil {
		a.Log.Error("building a class image failed", "err", err)
	}
}

// shutdown removes runners that have not taken a job, so GitHub hands their
// jobs to the next session. Jobs that are running keep running.
func (a *Agent) shutdown(s scheduler.State) {
	ctx, cancel := context.WithTimeout(context.Background(), stopBudget)
	defer cancel()
	var wg sync.WaitGroup
	for _, r := range s.Runners {
		if r.Phase == scheduler.Starting {
			wg.Go(func() { a.remove(ctx, r, true) })
		}
	}
	wg.Wait()
}

func (a *Agent) observe(ctx context.Context) {
	paths := []string{a.rootDir}
	if a.sysbox {
		paths = append(paths, a.SysboxDir)
	}
	for {
		free, path, err := disk.Lowest(paths...)
		if err != nil {
			a.Log.Error("reading free disk", "path", path, "err", err)
		}
		sizes := free < int64(a.Config.DiskFloor)/2
		list, err := a.Docker.List(ctx, layout.LabelBox+"="+a.Config.Box, sizes)
		if sizes && a.sysbox {
			withInnerDocker(list, a.SysboxDir)
		}
		if err == nil {
			a.post(scheduler.Observed{Containers: containers(list), DiskFree: free})
		} else if ctx.Err() == nil {
			a.Log.Error("listing containers", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * a.Tick):
		}
	}
}

// withInnerDocker adds to what Docker says each container wrote what Sysbox
// keeps for it under sysboxDir: the images and containers of a job's own
// Docker, which Docker's count leaves out.
func withInnerDocker(list []docker.Container, sysboxDir string) {
	for i, c := range list {
		if c.ID == "" {
			continue
		}
		dirs, _ := filepath.Glob(filepath.Join(sysboxDir, "*", c.ID))
		for _, dir := range dirs {
			list[i].SizeRw += disk.Used(dir)
		}
	}
}

func containers(list []docker.Container) []scheduler.Container {
	var out []scheduler.Container
	for _, c := range list {
		ssID, _ := strconv.ParseInt(c.Labels[layout.LabelScaleSetID], 10, 64)
		runnerID, _ := strconv.ParseInt(c.Labels[layout.LabelRunnerID], 10, 64)
		out = append(out, scheduler.Container{
			Runner:     c.Labels[layout.LabelRunner],
			ScaleSet:   scheduler.Key{Repo: c.Labels[layout.LabelRepo], Class: c.Labels[layout.LabelClass]},
			ScaleSetID: ssID,
			RunnerID:   runnerID,
			Running:    c.Running(),
			Created:    c.CreatedAt(),
			Disk:       c.SizeRw,
		})
	}
	return out
}

func (a *Agent) checkImage(ctx context.Context) {
	if built, err := runnerimage.Built(ctx, a.Docker); err != nil {
		a.Log.Error("reading the job image", "err", err)
	} else {
		if built != "" {
			a.ensureClasses(ctx)
		}
		a.post(scheduler.ImageBuilt{Version: built})
	}
	client := &http.Client{Timeout: 30 * time.Second}
	for {
		wait := imageEvery
		latest, err := runnerimage.Latest(ctx, client, a.ReleasesURL)
		if err != nil {
			a.Log.Warn("checking the runner's latest release", "err", err)
			wait = imageRetry
		} else {
			a.post(scheduler.LatestRunner{Version: runnerimage.Identity(latest)})
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (a *Agent) logJob(s scheduler.State, j queue.JobCompleted) {
	r, ok := s.Runners[j.RunnerName]
	if !ok {
		return
	}
	finished := j.FinishTime
	if finished.IsZero() {
		finished = time.Now()
	}
	err := status.Append(a.JobsLog, status.Job{
		Repo: r.ScaleSet.Repo, Class: r.ScaleSet.Class, Runner: r.Name,
		Workflow: j.WorkflowRef, Name: j.DisplayName, Result: j.Result,
		Queued: j.QueueTime, Started: j.RunnerAssignTime, Finished: finished,
	})
	if err != nil {
		a.Log.Warn("writing the job log", "err", err)
	}
	a.Log.Info("job finished", "repo", r.ScaleSet.Repo, "class", r.ScaleSet.Class, "job", j.DisplayName, "result", j.Result)
}

func (a *Agent) publish(s scheduler.State) {
	snap := status.Snapshot{
		Box: a.Config.Box, Version: a.Version,
		Image:    status.ImageStatus{Built: s.Image.Built, Latest: s.Image.Latest},
		DiskFree: s.DiskFree, DiskFloor: s.DiskFloor,
		Started: a.started, FreshStart: (a.adoptedNone && !a.ranBefore) || a.Worker.SessionBlocked(),
	}
	warm := scheduler.Warm(s)
	active := map[string]int{}
	for _, r := range s.Runners {
		if r.Phase != scheduler.Exited {
			active[r.ScaleSet.Class]++
		}
		snap.Runners = append(snap.Runners, status.Runner{Name: r.Name, Repo: r.ScaleSet.Repo, Class: r.ScaleSet.Class, Phase: r.Phase.String(), Since: r.Since, Warm: warm[r.Name]})
	}
	for _, c := range a.Config.Classes {
		snap.Classes = append(snap.Classes, status.Class{Name: c.Name, Priority: c.Priority.String(), Max: c.Max, Active: active[c.Name]})
	}
	for key, ss := range s.ScaleSets {
		snap.ScaleSets = append(snap.ScaleSets, status.ScaleSet{Repo: key.Repo, Class: key.Class, Assigned: len(ss.Jobs)})
	}
	slices.SortFunc(snap.Runners, func(x, y status.Runner) int { return cmp.Compare(x.Name, y.Name) })
	slices.SortFunc(snap.ScaleSets, func(x, y status.ScaleSet) int {
		return cmp.Or(cmp.Compare(x.Repo, y.Repo), cmp.Compare(x.Class, y.Class))
	})
	a.mu.Lock()
	a.snapshot = snap
	a.mu.Unlock()
}

func (a *Agent) status() status.Snapshot {
	a.mu.Lock()
	snap := a.snapshot
	a.mu.Unlock()
	jobs, err := status.Recent(a.JobsLog, 20)
	if err != nil {
		a.Log.Warn("reading the job log", "err", err)
	}
	snap.Jobs = jobs
	return snap
}

// supervise keeps one listener per served repository and class, re-reading
// the Worker's repositories every five minutes and whenever a heartbeat says
// the paused classes changed. A paused class keeps no listener, so it starts
// no runner; its running jobs finish and new ones wait at GitHub.
func (a *Agent) supervise(ctx context.Context) error {
	type exit struct {
		key scheduler.Key
		err error
	}
	running := map[scheduler.Key]context.CancelFunc{}
	exits := make(chan exit)
	paused := map[string]bool{}
	sync := func() error {
		repos, err := a.Worker.Repos(ctx)
		if err != nil {
			if worker.Fatal(err) {
				return err
			}
			if ctx.Err() == nil {
				a.Log.Error("listing repositories", "err", err)
			}
			return nil
		}
		want := map[scheduler.Key]bool{}
		for _, c := range a.Config.Classes {
			now := slices.Contains(repos.Paused, c.Name)
			switch {
			case now && !paused[c.Name]:
				a.Log.Info("class paused on the dashboard; starting no new runners for it", "class", c.Name)
			case !now && paused[c.Name]:
				a.Log.Info("class resumed on the dashboard", "class", c.Name)
			}
			paused[c.Name] = now
			if now {
				continue
			}
			if !slices.Contains(repos.Classes, c.Name) {
				a.Log.Error("class is not one of the Worker's classes; not serving it", "class", c.Name, "workerClasses", repos.Classes)
				continue
			}
			if c.Isolation == config.Sysbox && !a.sysbox {
				a.Log.Error("class needs Sysbox, for docker = true or isolation = \"sysbox\", but Docker has no "+layout.SysboxRuntime+" runtime; run `sudo boxrunner install`; not serving it", "class", c.Name)
				continue
			}
			for _, r := range repos.Repos {
				if c.Serves(r.Name) {
					want[scheduler.Key{Repo: r.Name, Class: c.Name}] = true
				}
			}
		}
		for key, cancel := range running {
			if !want[key] {
				if !paused[key.Class] {
					a.Log.Info("repository left the Worker's list; stopping", "repo", key.Repo, "class", key.Class)
				}
				cancel()
			}
		}
		for key := range want {
			if _, ok := running[key]; ok {
				continue
			}
			lctx, cancel := context.WithCancel(ctx)
			running[key] = cancel
			go func() { exits <- exit{key, a.listen(lctx, key)} }()
		}
		return nil
	}

	err := sync()
	tick := time.NewTicker(reposEvery)
	defer tick.Stop()
	for err == nil && ctx.Err() == nil {
		select {
		case <-tick.C:
			err = sync()
		case <-a.resync:
			err = sync()
		case e := <-exits:
			err = a.listenerExited(ctx, running, e.key, e.err)
		case <-ctx.Done():
		}
	}
	for _, cancel := range running {
		cancel()
	}
	for len(running) > 0 {
		e := <-exits
		delete(running, e.key)
	}
	return err
}

func (a *Agent) listenerExited(ctx context.Context, running map[scheduler.Key]context.CancelFunc, key scheduler.Key, err error) error {
	running[key]()
	delete(running, key)
	select {
	case a.events <- scheduler.ScaleSetGone{ScaleSet: key}:
	case <-ctx.Done():
	}
	switch {
	case err == nil:
	case worker.Fatal(err):
		return err
	case worker.Is(err, worker.RepoIneligible):
		a.Log.Warn("repository is public, archived or no longer has the App; stopped serving it", "repo", key.Repo, "err", err)
	default:
		a.Log.Error("listener stopped; it restarts within five minutes", "repo", key.Repo, "class", key.Class, "err", err)
	}
	return nil
}

func (a *Agent) listen(ctx context.Context, key scheduler.Key) error {
	ss, err := a.Worker.ScaleSet(ctx, key.Repo, key.Class)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	class, _ := a.Config.Class(key.Class)
	return listener.Run(ctx, listener.Config{
		ScaleSet: key, ScaleSetID: ss.ID, Owner: a.Config.Box, Capacity: class.Max,
		Worker: a.Worker, Queue: a.queue, Events: a.events, Log: a.Log,
	})
}

func hasEntries(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Size() > 0
}

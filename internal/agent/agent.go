// Package agent is the daemon behind `boxrunner run`. One goroutine owns the
// scheduler state; listeners, the Docker observer, the image checker and the
// executors only send it events.
package agent

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/cache"
	"github.com/dominikzabcik/boxrunner/internal/config"
	"github.com/dominikzabcik/boxrunner/internal/disk"
	"github.com/dominikzabcik/boxrunner/internal/dnsfwd"
	"github.com/dominikzabcik/boxrunner/internal/docker"
	"github.com/dominikzabcik/boxrunner/internal/egress"
	"github.com/dominikzabcik/boxrunner/internal/firewall"
	"github.com/dominikzabcik/boxrunner/internal/layout"
	"github.com/dominikzabcik/boxrunner/internal/listener"
	"github.com/dominikzabcik/boxrunner/internal/metrics"
	"github.com/dominikzabcik/boxrunner/internal/mirror"
	"github.com/dominikzabcik/boxrunner/internal/queue"
	"github.com/dominikzabcik/boxrunner/internal/release"
	"github.com/dominikzabcik/boxrunner/internal/runnerdigests"
	"github.com/dominikzabcik/boxrunner/internal/runnerimage"
	"github.com/dominikzabcik/boxrunner/internal/scheduler"
	"github.com/dominikzabcik/boxrunner/internal/status"
	"github.com/dominikzabcik/boxrunner/internal/worker"
)

const (
	reposEvery = 5 * time.Minute
	// An organization's repositories are listed every minute, as often as the
	// Worker reads them: the agent decides alone which offered jobs to acquire.
	orgReposEvery = time.Minute
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
	DrainFile      string
	Socket         string
	Runner         runnerdigests.Source
	Tick           time.Duration
	HeartbeatEvery time.Duration
	SysboxDir      string
	CgroupDir      string
	CacheDir       string
	// CacheAddr is where the Actions cache listens; zero means the job
	// network's gateway.
	CacheAddr netip.AddrPort
	// EgressListen opens the sockets of an egress class's DNS forwarder; nil
	// listens on port 53 of the class network's gateway.
	EgressListen func(egress.Net) (net.PacketConn, net.Listener, error)
	// OpenPackets reads what the egress rules log; nil opens the NFLOG group.
	OpenPackets func(group uint16) (PacketSource, error)
	// EgressUpstream answers the names an egress class allows; nil asks the
	// box's own resolvers.
	EgressUpstream dnsfwd.Upstream
	// EgressCountEvery is how often the rules' counters are read, and
	// EgressGuardEvery how often the table is checked; 0 means five and ten seconds.
	EgressCountEvery time.Duration
	EgressGuardEvery time.Duration
	// EgressRetry is how long an egress class's forwarder and the reader of
	// the rules' log first wait to start again after they stop; 0 means a
	// second. The wait doubles up to a minute.
	EgressRetry time.Duration
	// ReposEvery is how often the repositories are listed while the Worker
	// serves an organization; 0 means a minute.
	ReposEvery time.Duration

	// BoxrunnerReleasesURL is boxrunner's latest-release page.
	BoxrunnerReleasesURL string
}

type Agent struct {
	Options
	rootDir string
	// sysbox is whether Docker has Sysbox's runtime, which classes with
	// docker = true or isolation = "sysbox" need.
	sysbox bool
	// mirror is the registry mirror's URL for those jobs, "" when no class has Docker.
	mirror string
	// cache is the Actions cache's URL for classes with cache = true, "" when
	// it does not serve.
	cache string
	// jobRules are the job network's firewall rules, which the guard keeps.
	jobRules firewall.Rules
	// egress is what the agent keeps for the classes with an allowlist.
	egress *egressState
	queue  *queue.Client
	events chan scheduler.Event
	// stopped closes when the scheduler loop no longer reads events.
	stopped chan struct{}
	fatal   chan error
	work    sync.WaitGroup
	// resync asks supervise to list the repositories now, not at its next tick.
	resync chan struct{}
	// ids holds each scale set's id by scheduler.Key, so a box whose session is
	// refused can still start runners for its fleet share.
	ids sync.Map
	// usage holds the last cgroup sample of each running job container, by
	// runner name, for the job log.
	usage sync.Map
	// beat asks the heartbeat to report now: a scale set's jobs changed.
	beat chan struct{}

	// started, adoptedNone and finished belong to the scheduler goroutine.
	started     time.Time
	adoptedNone bool
	finished    []status.Tally
	// ranBefore is set once before any goroutine starts: the job log has
	// entries, so this box has run jobs and a start without containers is a
	// restart, not a new box taking over from a dead one.
	ranBefore bool

	mu       sync.Mutex
	snapshot status.Snapshot
	// imageDisk is what boxrunner's images take on disk after the last prune.
	imageDisk atomic.Int64
	// building is the job image the agent is building, or whose class images
	// it is building, nil while it builds nothing.
	building atomic.Pointer[string]
	// latest is boxrunner's latest release, once checkRelease found it.
	latest atomic.Pointer[string]
	// heartbeatOK is when the Worker last took a heartbeat, in Unix nanoseconds.
	heartbeatOK atomic.Int64
	// org is the organization the Worker serves, nil while it serves
	// repositories one by one. accepts holds, by class, the repositories (lower
	// case) whose jobs the class's organization scale set acquires.
	org     atomic.Pointer[worker.Organization]
	accepts atomic.Pointer[map[string]map[string]bool]
	// refused counts, by scale set and repository, the jobs an organization
	// scale set was offered from repositories the box does not serve.
	refusedMu sync.Mutex
	refused   map[scheduler.Key]map[string]int
	// served changes whenever accepts does, so the listeners read the jobs
	// waiting for their scale sets again.
	served atomic.Int64
	// pending holds, by organization scale set, the repositories of the jobs
	// it holds that no runner has taken, which a mint names to the Worker.
	pendingMu sync.Mutex
	pending   map[scheduler.Key][]string
	// draining mirrors DrainFile; drainMu orders the changes to both.
	draining atomic.Bool
	drainMu  sync.Mutex
}

// Run serves jobs until ctx ends. It returns an error only for a problem a
// restart cannot fix, such as a wrong box token.
func Run(ctx context.Context, o Options) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	o.JobsLog = cmp.Or(o.JobsLog, layout.JobsLogPath)
	o.DrainFile = cmp.Or(o.DrainFile, layout.DrainPath)
	o.Socket = cmp.Or(o.Socket, layout.Socket)
	o.BoxrunnerReleasesURL = cmp.Or(o.BoxrunnerReleasesURL, release.Releases+"/latest")
	o.Tick = cmp.Or(o.Tick, decideEvery)
	o.HeartbeatEvery = cmp.Or(o.HeartbeatEvery, heartbeatEvery)
	o.SysboxDir = cmp.Or(o.SysboxDir, layout.SysboxDir)
	o.CgroupDir = cmp.Or(o.CgroupDir, layout.CgroupDir)
	o.CacheDir = cmp.Or(o.CacheDir, layout.CacheDir)
	a := &Agent{
		Options: o,
		queue:   &queue.Client{HTTP: &http.Client{Timeout: 90 * time.Second}, UserAgent: "boxrunner/" + o.Version},
		events:  make(chan scheduler.Event, 64),
		started: time.Now(),
		stopped: make(chan struct{}),
		fatal:   make(chan error, 1),
		resync:  make(chan struct{}, 1),
		beat:    make(chan struct{}, 1),
		egress:  &egressState{enforcer: &egress.Enforcer{Run: o.Run}, ledger: &egress.Ledger{}},
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
	// The cache outlives the rest of the agent's shutdown, so that a job
	// whose step talks to it while the agent stops gets its answer.
	cacheDone := a.serveCache(context.WithoutCancel(ctx))
	defer cacheDone()
	// Jobs that run while the agent stops keep their DNS until it exits.
	egressDone := a.serveEgress(context.WithoutCancel(ctx))
	defer egressDone()
	if err := os.MkdirAll(filepath.Dir(a.JobsLog), 0o755); err != nil {
		return err
	}
	a.ranBefore = hasEntries(a.JobsLog)
	_, err = os.Stat(a.DrainFile)
	a.draining.Store(err == nil)
	a.Log.Info("boxrunner started", "box", a.Config.Box, "version", a.Version, "worker", a.Config.Worker.String(), "draining", a.draining.Load())

	// Status answers from the first snapshot on, not an empty one before the
	// scheduler's first turn.
	state := scheduler.Apply(scheduler.New(a.Config), scheduler.Drain{On: a.draining.Load()}, time.Now())
	a.publish(state)
	served := make(chan struct{})
	go func() {
		defer close(served)
		if err := status.Serve(ctx, a.Socket, a.status, a.drain); err != nil {
			a.Log.Error("status socket", "err", err)
		}
	}()
	if addr := a.Config.Metrics; addr.IsValid() {
		if l, err := net.Listen("tcp", addr.String()); err != nil {
			a.Log.Error("serving metrics", "addr", addr, "err", err)
		} else {
			go func() {
				if err := metrics.Serve(ctx, l, a.current); err != nil {
					a.Log.Error("serving metrics", "addr", addr, "err", err)
				}
			}()
		}
	}
	go a.observe(ctx)
	go a.heartbeat(ctx)
	go a.checkImage(ctx)
	go a.checkRelease(ctx)
	supervised := make(chan struct{})
	go func() {
		defer close(supervised)
		if err := a.supervise(ctx); err != nil {
			a.raise(err)
		}
	}()

	err = a.schedule(ctx, state)
	cancel()
	<-supervised
	<-served
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
// could have lost: the idle slice's cpu.idle, the network, the registry
// mirror for classes with Docker and the firewall rules.
func (a *Agent) converge(ctx context.Context) error {
	a.idleSlice(ctx)
	if _, ok, err := a.Docker.Network(ctx, layout.Network); err != nil {
		return err
	} else if !ok {
		if err := a.Docker.CreateNetwork(ctx, docker.NetworkSpec{Name: layout.Network, Bridge: layout.Bridge}); err != nil {
			return fmt.Errorf("creating network %s: %w", layout.Network, err)
		}
	}
	services := firewall.Services{
		Mirror: slices.ContainsFunc(a.Config.Classes, func(c config.Class) bool { return c.Docker }),
		Cache:  a.Config.Cache > 0,
	}
	rules := firewall.Rules{Bridge: layout.Bridge}
	var gateway netip.Addr
	if services != (firewall.Services{}) {
		var err error
		if gateway, err = a.Docker.Gateway(ctx, layout.Network); err != nil {
			return err
		}
		rules = services.Rules(gateway)
		if services.Mirror {
			addr := mirror.Addr(gateway)
			// Without the mirror, a job's Docker pulls from Docker Hub itself.
			if err := mirror.Ensure(ctx, a.Docker, addr); err != nil {
				a.Log.Error("starting the registry mirror failed; jobs pull from Docker Hub directly", "err", err)
			}
			a.mirror = mirror.URL(addr)
		}
		if services.Cache && !a.CacheAddr.IsValid() {
			a.CacheAddr = cache.Addr(gateway)
		}
	}
	if err := firewall.Apply(ctx, a.Run, rules); err != nil {
		return fmt.Errorf("firewall: %w", err)
	}
	a.jobRules = rules
	a.convergeEgress(ctx, services, gateway)
	return nil
}

// serveCache starts the Actions cache when the config has one, and returns
// a function that stops it. Without it, jobs use GitHub's cache.
func (a *Agent) serveCache(ctx context.Context) func() {
	if a.Config.Cache == 0 {
		return func() {}
	}
	store := &cache.Store{
		Dir: a.CacheDir, Max: int64(a.Config.Cache), Floor: int64(a.Config.DiskFloor),
		Free: func() (int64, error) { return disk.Free(a.CacheDir) },
	}
	err := store.Open()
	var l net.Listener
	if err == nil {
		l, err = net.Listen("tcp", a.CacheAddr.String())
	}
	if err != nil {
		a.Log.Error("serving the Actions cache failed; jobs use GitHub's cache", "addr", a.CacheAddr, "err", err)
		return func() {}
	}
	upstream, _ := url.Parse(cache.Upstream)
	srv := &cache.Server{
		Store: store, Base: cache.URL(a.CacheAddr), Upstream: upstream, Log: a.Log, Now: time.Now,
		Keys: &cache.Keys{URL: cache.KeysURL, Issuer: cache.Issuer, HTTP: &http.Client{Timeout: 30 * time.Second}, Log: a.Log},
	}
	a.cache = srv.Base
	size, entries := store.Size()
	a.Log.Info("serving the Actions cache", "url", srv.Base, "entries", entries, "bytes", size, "max", a.Config.Cache.String())
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(ctx, l); err != nil {
			a.Log.Error("serving the Actions cache", "err", err)
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// idleSlice starts the idle slice and sets its cpu.idle to 1. systemd does
// that for CPUWeight=idle only from version 252, and a new cgroup starts at 0,
// so the agent sets it after every reboot and restart of the slice.
func (a *Agent) idleSlice(ctx context.Context) {
	file := filepath.Join(a.CgroupDir, layout.IdleSlice, "cpu.idle")
	if b, err := os.ReadFile(file); err == nil && strings.TrimSpace(string(b)) == "1" {
		return
	}
	err := func() error {
		if out, err := a.Run(ctx, "", "systemctl", "start", layout.IdleSlice); err != nil {
			return fmt.Errorf("systemctl start %s: %w: %s", layout.IdleSlice, err, out)
		}
		// Not os.WriteFile: where the kernel has no cpu.idle, creating it
		// fails as "permission denied", which hides why.
		f, err := os.OpenFile(file, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		_, err = f.WriteString("1")
		return errors.Join(err, f.Close())
	}()
	if err != nil {
		a.Log.Error("the idle slice is not idle; run boxrunner doctor", "err", err)
	}
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

// drain closes the sessions and removes the runners without a job, or with
// on false opens the sessions again. The file keeps a drain across restarts.
func (a *Agent) drain(on bool) error {
	a.drainMu.Lock()
	defer a.drainMu.Unlock()
	var err error
	if on {
		err = os.WriteFile(a.DrainFile, nil, 0o644)
	} else if err = os.Remove(a.DrainFile); errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err != nil {
		return err
	}
	if a.draining.Swap(on) != on {
		a.Log.Info(map[bool]string{true: "draining: closing sessions; running jobs keep running", false: "resuming: opening sessions"}[on])
	}
	a.post(scheduler.Drain{On: on})
	select {
	case a.resync <- struct{}{}:
	default:
	}
	return nil
}

func (a *Agent) schedule(ctx context.Context, state scheduler.State) error {
	defer close(a.stopped)
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
				if len(u.Jobs) != len(state.ScaleSets[u.ScaleSet].Jobs) {
					select {
					case a.beat <- struct{}{}:
					default:
					}
				}
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
		if act.Logs {
			b := s.Backoff[r.ScaleSet.Class]
			a.Log.Warn("runners of this class exit before taking a job; holding new starts", "class", r.ScaleSet.Class, "failures", b.Failures, "nextTry", b.Until)
		}
		s = scheduler.Apply(s, scheduler.RemoveDispatched{Runner: act.Runner}, now)
		a.spawn(func() { a.remove(work, r, act.Deregister, act.Logs) })
	case scheduler.Build:
		s = scheduler.Apply(s, scheduler.BuildDispatched{}, now)
		a.spawn(func() { a.build(ctx, act.Version, act.Digest) })
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
// through mirror when one is given. A class with a cache gets the cache's URL
// when the box serves one. A class with an allowlist runs on its own network
// and resolves names through its forwarder.
func Spec(box string, class config.Class, r scheduler.Runner, jitConfig, mirror, cache string, net *egress.Net) docker.ContainerSpec {
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
	if class.Cache && cache != "" {
		spec.Env = append(spec.Env, layout.EnvCache+"="+cache)
	}
	if net != nil {
		// The job's resolver forwards to the class's forwarder, and so does
		// the daemon of a job that has its own Docker.
		spec.Network, spec.DNS = net.Name, []string{net.Gateway.String()}
		if class.Docker {
			spec.Env = append(spec.Env, layout.EnvDNS+"="+net.Gateway.String())
		}
	}
	return spec
}

func (a *Agent) start(ctx context.Context, r scheduler.Runner) {
	ctx, cancel := context.WithTimeout(ctx, scheduler.IdleTimeout)
	defer cancel()
	log := a.Log.With("runner", r.Name, "repo", r.ScaleSet.Repo)
	class, _ := a.Config.Class(r.ScaleSet.Class)
	var egressNet *egress.Net
	if class.Egress != nil {
		// A job of an egress class never starts on a box whose rules are gone.
		if err := a.verifyEgress(ctx, true); err != nil {
			log.Error("the egress rules are not in place; not starting a runner", "class", class.Name, "err", err)
			a.post(scheduler.StartFailed{Runner: r.Name})
			return
		}
		// Nor on the job network, which its rules do not cover.
		if egressNet = a.egressNet(class.Name); egressNet == nil {
			problem, _ := a.egressProblem(class.Name)
			log.Error("the class's egress allowlist is not in force; not starting a runner", "class", class.Name, "problem", problem)
			a.post(scheduler.StartFailed{Runner: r.Name})
			return
		}
	}
	minted, err := a.Worker.MintRunner(ctx, r.ScaleSet.Repo, r.ScaleSetID, r.Name, a.pendingRepos(r.ScaleSet))
	if err != nil {
		if worker.IsOrg(r.ScaleSet.Repo) && worker.Is(err, worker.RepoIneligible) {
			// Such a job stays with the scale set until someone cancels it, so the class backs off instead of asking every tick.
			log.Error("the Worker refuses a runner while the organization's scale set holds a job of a repository it does not serve; cancel that job on GitHub", "class", class.Name, "err", err)
			a.post(scheduler.StartFailed{Runner: r.Name, Container: true})
			return
		}
		log.Error("minting a runner config failed", "err", err)
		if worker.Fatal(err) {
			a.raise(err)
		}
		a.post(scheduler.StartFailed{Runner: r.Name})
		return
	}
	r.ID = minted.ID
	if class.Priority == config.Idle {
		a.idleSlice(ctx)
	}
	id, err := a.Docker.Create(ctx, Spec(a.Config.Box, class, r, minted.JITConfig, a.mirror, a.cache, egressNet))
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
		a.post(scheduler.StartFailed{Runner: r.Name, Container: true})
		return
	}
	log.Info("runner started", "class", class.Name)
	a.post(scheduler.Started{Runner: r.Name, ID: r.ID})
}

// tailLines is how much of a failed container's output the agent logs.
const tailLines = 20

func (a *Agent) remove(ctx context.Context, r scheduler.Runner, deregister, logs bool) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if logs {
		lines, err := a.Docker.Tail(ctx, r.Name, tailLines)
		if err != nil {
			a.Log.Warn("reading the exited container's output failed", "runner", r.Name, "err", err)
		} else {
			a.Log.Warn("the exited container's last lines", "runner", r.Name, "class", r.ScaleSet.Class, "output", strings.Join(lines, "\n"))
		}
	}
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
	a.usage.Delete(r.Name)
	a.post(scheduler.Removed{Runner: r.Name})
}

const stopGrace = 30 * time.Second

func (a *Agent) build(ctx context.Context, version, digest string) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	a.Log.Info("building the job image", "runner", version, "digest", digest)
	a.building.Store(&version)
	err := runnerimage.Build(ctx, a.Docker, a.Runner, version, digest)
	if err == nil {
		a.ensureClasses(ctx)
	}
	// Cleared before the scheduler hears the outcome, so that no snapshot
	// shows the outcome and the build at once.
	a.building.Store(nil)
	var mismatch *runnerdigests.Mismatch
	switch {
	case errors.As(err, &mismatch):
		a.Log.Error("refused to build the job image; jobs keep the current image", "runner", version, "reason", err)
		a.post(scheduler.BuildFailed{Refused: err.Error()})
		return
	case err != nil:
		a.Log.Error("building the job image failed", "runner", version, "err", err)
		a.post(scheduler.BuildFailed{})
		return
	}
	a.post(scheduler.ImageBuilt{Version: version, Digest: digest})
}

// ensureClasses rebuilds the class images whose job image or files changed,
// then removes the images those builds and earlier ones superseded.
func (a *Agent) ensureClasses(ctx context.Context) {
	built, err := runnerimage.EnsureClasses(ctx, a.Docker, a.Config.Classes)
	for _, class := range built {
		a.Log.Info("built the class image", "class", class)
	}
	if err != nil {
		a.Log.Error("building a class image failed", "err", err)
	}
	removed, disk, err := runnerimage.Prune(ctx, a.Docker)
	for _, ref := range removed {
		a.Log.Info("removed a superseded image", "image", ref)
	}
	if err != nil {
		a.Log.Warn("removing superseded images", "err", err)
	}
	a.imageDisk.Store(disk)
}

// shutdown removes runners that have not taken a job, so GitHub hands their
// jobs to the next session. Jobs that are running keep running, except those
// of egress classes: with the agent gone nobody puts their rules back, so
// they are stopped, and their jobs cancelled, as the forwarder still answers.
func (a *Agent) shutdown(s scheduler.State) {
	ctx, cancel := context.WithTimeout(context.Background(), stopBudget)
	defer cancel()
	var wg sync.WaitGroup
	for _, r := range s.Runners {
		if r.Phase == scheduler.Starting {
			wg.Go(func() { a.remove(ctx, r, true, false) })
		}
	}
	a.egress.mu.Lock()
	egressClasses := len(a.egress.classes) > 0 || len(a.egress.ruleset.Classes) > 0
	a.egress.mu.Unlock()
	if egressClasses {
		wg.Go(func() { a.stopEgressJobs(ctx, stopGrace, "the agent is stopping") })
	}
	wg.Wait()
}

func (a *Agent) observe(ctx context.Context) {
	paths := []string{a.rootDir}
	if a.sysbox {
		paths = append(paths, a.SysboxDir)
	}
	for {
		if a.sysbox {
			a.sweepSysbox(ctx)
		}
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
			a.sampleUsage(list)
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
		if built.Identity != "" {
			// Runners wait for the class images, which a changed class or an
			// interrupted build leaves to build now.
			a.building.Store(&built.Identity)
			a.ensureClasses(ctx)
			a.building.Store(nil)
		}
		a.post(scheduler.ImageBuilt{Version: built.Identity, Digest: built.Digest})
	}
	for {
		wait := imageEvery
		choice, err := a.Runner.Choose(ctx)
		switch {
		case err != nil:
			a.Log.Warn("choosing the runner image; jobs keep the current image", "err", err)
			wait = imageRetry
		default:
			if choice.Unlisted != "" {
				a.Log.Info("boxrunner's CI has not verified the runner's latest release yet", "latest", choice.Unlisted, "building", choice.Version)
				wait = imageRetry
			}
			a.post(scheduler.LatestRunner{Version: runnerimage.Identity(choice.Version), Digest: choice.Digest, Unlisted: choice.Unlisted})
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
	// A job of an organization scale set is recorded under its own repository.
	repo := r.ScaleSet.Repo
	if worker.IsOrg(repo) {
		repo = cmp.Or(j.FullName(repo), repo)
	}
	job := status.Job{
		Repo: repo, Class: r.ScaleSet.Class, Runner: r.Name,
		Workflow: j.WorkflowRef, Name: j.DisplayName, Result: j.Result,
		Queued: j.QueueTime, Started: j.RunnerAssignTime, Finished: finished,
	}
	if u, ok := a.usage.Load(r.Name); ok {
		used := u.(status.Usage)
		job.Usage = &used
	}
	a.finished = status.Count(a.finished, job)
	if err := status.Append(a.JobsLog, job); err != nil {
		a.Log.Warn("writing the job log", "err", err)
	}
	a.Log.Info("job finished", "repo", repo, "class", r.ScaleSet.Class, "job", j.DisplayName, "result", j.Result)
}

func (a *Agent) publish(s scheduler.State) {
	workerVersion, _ := a.Worker.WorkerVersion()
	snap := status.Snapshot{
		Box: a.Config.Box, Version: a.Version, Worker: workerVersion,
		Image: status.ImageStatus{
			Built: s.Image.Built, Digest: s.Image.Digest, Latest: s.Image.Latest, Unlisted: s.Image.Unlisted,
			Refused: s.Image.Refused, Disk: a.imageDisk.Load(),
		},
		DiskFree: s.DiskFree, DiskFloor: s.DiskFloor,
		Started: a.started, FreshStart: (a.adoptedNone && !a.ranBefore) || a.Worker.SessionBlocked(),
		Draining: s.Draining, Finished: a.finished,
	}
	if ns := a.heartbeatOK.Load(); ns != 0 {
		snap.Heartbeat = time.Unix(0, ns)
	}
	if latest := a.latest.Load(); latest != nil {
		snap.Latest = *latest
	}
	if building := a.building.Load(); building != nil {
		snap.Image.Building = *building
	}
	if org := a.org.Load(); org != nil {
		snap.Organization = &status.Organization{Name: org.Name, Group: org.Group, Problem: org.Problem}
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
		snap.Classes = append(snap.Classes, status.Class{Name: c.Name, Priority: c.Priority.String(), Max: c.Max, Active: active[c.Name], HeldUntil: s.Backoff[c.Name].Until})
	}
	a.refusedMu.Lock()
	for key, ss := range s.ScaleSets {
		snap.ScaleSets = append(snap.ScaleSets, status.ScaleSet{Repo: key.Repo, Class: key.Class, Assigned: len(ss.Jobs), Refused: maps.Clone(a.refused[key])})
	}
	a.refusedMu.Unlock()
	for key, box := range s.HeldBy {
		share, fleet := s.Fleet[key]
		snap.Elsewhere = append(snap.Elsewhere, status.Elsewhere{Repo: key.Repo, Class: key.Class, Box: box, Fleet: fleet, Share: share})
	}
	snap.Egress = a.egressView()
	slices.SortFunc(snap.Elsewhere, func(x, y status.Elsewhere) int {
		return cmp.Or(cmp.Compare(x.Repo, y.Repo), cmp.Compare(x.Class, y.Class))
	})
	slices.SortFunc(snap.Runners, func(x, y status.Runner) int { return cmp.Compare(x.Name, y.Name) })
	slices.SortFunc(snap.ScaleSets, func(x, y status.ScaleSet) int {
		return cmp.Or(cmp.Compare(x.Repo, y.Repo), cmp.Compare(x.Class, y.Class))
	})
	a.mu.Lock()
	a.snapshot = snap
	a.mu.Unlock()
}

// current is the last snapshot the scheduler published.
func (a *Agent) current() status.Snapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.snapshot
}

func (a *Agent) status() status.Snapshot {
	snap := a.current()
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
	// held are the stopped listeners whose scale set another box's session holds.
	held := map[scheduler.Key]bool{}
	exits := make(chan exit)
	paused := map[string]bool{}
	sync := func() error {
		want := map[scheduler.Key]bool{}
		draining := a.draining.Load()
		if !draining {
			var err error
			if want, err = a.wanted(ctx, paused); want == nil {
				return err
			}
		}
		for key, cancel := range running {
			if !want[key] {
				if !paused[key.Class] && !draining {
					a.Log.Info("repository left the Worker's list; stopping", "repo", key.Repo, "class", key.Class)
				}
				cancel()
			}
		}
		for key := range held {
			if !want[key] {
				delete(held, key)
				a.post(scheduler.ScaleSetGone{ScaleSet: key})
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

	every := func() time.Duration {
		if a.org.Load() != nil {
			return cmp.Or(a.ReposEvery, orgReposEvery)
		}
		return reposEvery
	}
	err := sync()
	tick := time.NewTicker(every())
	defer tick.Stop()
	for err == nil && ctx.Err() == nil {
		select {
		case <-tick.C:
			err = sync()
			tick.Reset(every())
		case <-a.resync:
			err = sync()
			tick.Reset(every())
		case e := <-exits:
			if worker.HeldBy(e.err) != "" {
				held[e.key] = true
			} else {
				delete(held, e.key)
			}
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

// wanted lists the repositories and classes to serve, logging the classes
// whose pause changed. It returns nil when the Worker did not answer, with an
// error only when retrying cannot help.
func (a *Agent) wanted(ctx context.Context, paused map[string]bool) (map[scheduler.Key]bool, error) {
	repos, err := a.Worker.Repos(ctx)
	if err != nil {
		if worker.Fatal(err) {
			return nil, err
		}
		if ctx.Err() == nil {
			a.Log.Error("listing repositories", "err", err)
		}
		return nil, nil
	}
	want := map[scheduler.Key]bool{}
	org := repos.Organization
	a.org.Store(org)
	if org != nil && org.Problem != "" {
		a.Log.Error("the Worker serves no repository of its organization", "org", org.Name, "group", org.Group, "problem", org.Problem)
	}
	accepts := map[string]map[string]bool{}
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
		if !a.egressServes(c) {
			a.Log.Error("class has an egress allowlist that is not in force; not serving it", "class", c.Name)
			continue
		}
		if c.Isolation == config.Sysbox && !a.sysbox {
			a.Log.Error("class needs Sysbox, for docker = true or isolation = \"sysbox\", but Docker has no "+layout.SysboxRuntime+" runtime; run `sudo boxrunner install`; not serving it", "class", c.Name)
			continue
		}
		for _, r := range repos.Repos {
			switch {
			case !c.Serves(r.Name):
			case org == nil:
				want[scheduler.Key{Repo: r.Name, Class: c.Name}] = true
			default:
				// One scale set per class serves the whole organization.
				want[scheduler.Key{Repo: org.Name, Class: c.Name}] = true
				if accepts[c.Name] == nil {
					accepts[c.Name] = map[string]bool{}
				}
				accepts[c.Name][strings.ToLower(r.Name)] = true
			}
		}
	}
	if old := a.accepts.Swap(&accepts); old == nil || !maps.EqualFunc(*old, accepts, maps.Equal) {
		a.served.Add(1)
	}
	return want, nil
}

// pendingRepos are the repositories of the jobs the organization scale set
// holds that no runner has taken; nil for a repository's scale set.
func (a *Agent) pendingRepos(key scheduler.Key) []string {
	if !worker.IsOrg(key.Repo) {
		return nil
	}
	a.pendingMu.Lock()
	defer a.pendingMu.Unlock()
	return a.pending[key]
}

func (a *Agent) setPending(key scheduler.Key, repos []string) {
	a.pendingMu.Lock()
	defer a.pendingMu.Unlock()
	if a.pending == nil {
		a.pending = map[scheduler.Key][]string{}
	}
	a.pending[key] = repos
}

// accept reports whether the class's organization scale set acquires a job
// of repo: the Worker serves the repository and so does the class.
func (a *Agent) accept(class, repo string) bool {
	accepts := a.accepts.Load()
	return accepts != nil && (*accepts)[class][strings.ToLower(repo)]
}

// refuse records an offered job of a repository the box does not serve,
// which waits at GitHub for a box that does.
func (a *Agent) refuse(key scheduler.Key, j queue.Job) {
	repo := cmp.Or(j.FullName(key.Repo), "an unnamed repository")
	a.Log.Warn("the organization's scale set was offered a job of a repository this box does not serve; it waits at GitHub", "org", key.Repo, "class", key.Class, "repo", repo, "job", j.DisplayName)
	a.refusedMu.Lock()
	defer a.refusedMu.Unlock()
	if a.refused == nil {
		a.refused = map[scheduler.Key]map[string]int{}
	}
	if a.refused[key] == nil {
		a.refused[key] = map[string]int{}
	}
	a.refused[key][repo]++
}

func (a *Agent) listenerExited(ctx context.Context, running map[scheduler.Key]context.CancelFunc, key scheduler.Key, err error) error {
	running[key]()
	delete(running, key)
	heldBy := worker.HeldBy(err)
	select {
	case a.events <- scheduler.ScaleSetGone{ScaleSet: key, HeldBy: heldBy, ID: a.scaleSetID(key)}:
	case <-ctx.Done():
	}
	switch {
	case err == nil:
	case heldBy != "":
		a.Log.Info("another box's session holds the scale set; trying again within five minutes", "repo", key.Repo, "class", key.Class, "box", heldBy)
	case worker.Fatal(err):
		return err
	case worker.Is(err, worker.RepoIneligible):
		a.Log.Warn("repository is public, archived or no longer has the App; stopped serving it", "repo", key.Repo, "err", err)
	case worker.Is(err, worker.GroupIneligible):
		a.Log.Error("the Worker refuses the organization's runner group; trying again within five minutes", "org", key.Repo, "class", key.Class, "err", err)
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
	a.ids.Store(key, ss.ID)
	class, _ := a.Config.Class(key.Class)
	c := listener.Config{
		ScaleSet: key, ScaleSetID: ss.ID, Owner: a.Config.Box, Capacity: class.Max,
		Worker: a.Worker, Queue: a.queue, Events: a.events, Log: a.Log,
	}
	if worker.IsOrg(key.Repo) {
		c.Accept = func(j queue.Job) bool { return a.accept(key.Class, j.FullName(key.Repo)) }
		c.Refused = func(j queue.Job) { a.refuse(key, j) }
		c.Acquirable = func(ctx context.Context) ([]queue.Job, error) { return a.Worker.AcquirableJobs(ctx, key.Repo, ss.ID) }
		c.Served = a.served.Load
		c.Pending = func(repos []string) { a.setPending(key, repos) }
	}
	return listener.Run(ctx, c)
}

// scaleSetID is the id the Worker last gave the scale set; 0 before it did.
func (a *Agent) scaleSetID(key scheduler.Key) int64 {
	id, _ := a.ids.Load(key)
	n, _ := id.(int64)
	return n
}

func hasEntries(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Size() > 0
}

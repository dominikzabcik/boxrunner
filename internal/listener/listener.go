// Package listener serves one scale set: it long-polls the queue with the
// session token, acquires every offered job, acknowledges each message and
// tells the scheduler what it learned. It never touches runners.
package listener

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/backoff"
	"github.com/dominikzabcik/boxrunner/internal/queue"
	"github.com/dominikzabcik/boxrunner/internal/scheduler"
	"github.com/dominikzabcik/boxrunner/internal/worker"
)

// RefreshAfter is when a session is refreshed before its token expires; the
// token lives 70 minutes (docs/decisions.md).
const RefreshAfter = 60 * time.Minute

type Worker interface {
	OpenSession(ctx context.Context, repo string, scaleSetID int64, owner string) (worker.Session, error)
	RefreshSession(ctx context.Context, repo string, scaleSetID int64, sessionID, owner string) (worker.Session, error)
	CloseSession(ctx context.Context, repo string, scaleSetID int64, sessionID string) error
}

type Config struct {
	ScaleSet   scheduler.Key
	ScaleSetID int64
	// Owner is the box name; GitHub shows it as the session's owner.
	Owner string
	// Capacity is the class's max, sent to GitHub with every poll.
	Capacity int
	Worker   Worker
	Queue    *queue.Client
	Events   chan<- scheduler.Event
	Log      *slog.Logger
	Now      func() time.Time
	Backoff  backoff.Policy
	// Accept, when set, decides which offered jobs the listener acquires. An
	// organization scale set is offered the jobs of every repository its runner
	// group admits, and the box serves only some of them. A job it refuses stays
	// queued at GitHub, and Refused hears of it once.
	Accept  func(queue.Job) bool
	Refused func(queue.Job)
	// Acquirable, when set, lists the jobs the scale set may still acquire.
	// GitHub offers a job in one message, so the listener asks when a session
	// opens and whenever Served changes, and acquires the ones Accept takes
	// now, such as a job it turned down while the box's list was out of date.
	Acquirable func(context.Context) ([]queue.Job, error)
	// Served changes whenever the repositories Accept takes change.
	Served func() int64
	// Pending, when set, hears the repositories (owner/name) of the jobs the
	// scale set holds that no runner has taken, after each message.
	Pending func([]string)
}

type listener struct {
	Config
	session   worker.Session
	opened    time.Time
	refreshAt time.Time
	// jobs maps each job assigned to the scale set to its queue time.
	jobs map[int64]time.Time
	// refused are the offered jobs Accept turned down, so each is reported once.
	refused map[int64]bool
	// pending maps each job the scale set holds that no runner took to its repository.
	pending map[int64]string
	// served is Served's value when the acquirable jobs were last read;
	// recheck asks to read them before the next poll.
	served  int64
	recheck bool
}

// Run serves the scale set until ctx ends, then closes the session. It
// returns early only with a Worker error that retrying cannot fix, such as a
// repository that became public.
func Run(ctx context.Context, c Config) error {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Log == nil {
		c.Log = slog.New(slog.DiscardHandler)
	}
	l := &listener{Config: c, jobs: map[int64]time.Time{}, refused: map[int64]bool{}, pending: map[int64]string{}}
	if err := l.open(ctx); err != nil {
		return quiet(ctx, err)
	}
	defer l.close()
	l.Log.Info("listening", "repo", c.ScaleSet.Repo, "class", c.ScaleSet.Class, "scaleSetId", c.ScaleSetID)
	if err := l.publish(ctx, nil, &l.session.Statistics); err != nil {
		return nil
	}

	var lastID int64
	for attempt := 0; ctx.Err() == nil; {
		err := l.poll(ctx, &lastID)
		var status *queue.StatusError
		switch {
		case err == nil:
			attempt = 0
			continue
		case ctx.Err() != nil:
			return nil
		case errors.Is(err, queue.ErrTokenExpired):
			err = l.refresh(ctx)
		case errors.As(err, &status) && status.Status == http.StatusNotFound:
			l.Log.Warn("session gone, opening a new one", "repo", c.ScaleSet.Repo, "class", c.ScaleSet.Class)
			err = l.open(ctx)
		}
		if err == nil {
			continue
		}
		var werr *worker.Error
		if errors.As(err, &werr) {
			return quiet(ctx, err)
		}
		l.Log.Warn("queue call failed, retrying", "repo", c.ScaleSet.Repo, "class", c.ScaleSet.Class, "attempt", attempt+1, "err", err)
		if l.Backoff.Wait(ctx, attempt) != nil {
			return nil
		}
		attempt++
	}
	return nil
}

// quiet drops errors caused by shutting down.
func quiet(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func (l *listener) poll(ctx context.Context, lastID *int64) error {
	if !l.Now().Before(l.refreshAt) {
		if err := l.refresh(ctx); err != nil {
			return err
		}
	}
	if l.Acquirable != nil && (l.recheck || l.served != l.servedNow()) {
		l.acquireWaiting(ctx)
	}
	msg, err := l.Queue.Get(ctx, l.session.Queue, *lastID, l.Capacity)
	if err != nil || msg == nil {
		return err
	}
	msg.Available = l.accepted(msg.Available)
	if len(msg.Available) > 0 {
		ids := make([]int64, len(msg.Available))
		for i, j := range msg.Available {
			ids[i] = j.RequestID
		}
		if _, err := l.Queue.Acquire(ctx, l.session.Queue, ids); err != nil {
			return err
		}
	}
	// The message is handled; an ack must not be lost to shutdown.
	if err := l.Queue.Ack(context.WithoutCancel(ctx), l.session.Queue, msg.ID); err != nil {
		return err
	}
	*lastID = msg.ID
	if l.publish(ctx, msg, msg.Statistics) != nil {
		return ctx.Err()
	}
	return nil
}

func (l *listener) servedNow() int64 {
	if l.Served == nil {
		return 0
	}
	return l.Served()
}

// acquireWaiting acquires the waiting jobs Accept now takes. A failure is
// tried again before the next poll.
func (l *listener) acquireWaiting(ctx context.Context) {
	served := l.servedNow()
	jobs, err := l.Acquirable(ctx)
	if err == nil {
		jobs = l.accepted(jobs)
		if len(jobs) > 0 {
			ids := make([]int64, len(jobs))
			for i, j := range jobs {
				ids[i] = j.RequestID
			}
			_, err = l.Queue.Acquire(ctx, l.session.Queue, ids)
		}
	}
	if err != nil {
		if ctx.Err() == nil {
			l.Log.Warn("reading the jobs waiting for the scale set failed; trying again before the next poll", "repo", l.ScaleSet.Repo, "class", l.ScaleSet.Class, "err", err)
		}
		l.recheck = true
		return
	}
	l.served, l.recheck = served, false
	for _, j := range jobs {
		l.track(j)
		l.hold(j)
	}
	if len(jobs) > 0 {
		l.Log.Info("acquired jobs that waited for the scale set", "repo", l.ScaleSet.Repo, "class", l.ScaleSet.Class, "jobs", len(jobs))
	}
}

// accepted drops the offered jobs Accept refuses, reporting each one once.
func (l *listener) accepted(offered []queue.Job) []queue.Job {
	if l.Accept == nil {
		return offered
	}
	var kept []queue.Job
	for _, j := range offered {
		if l.Accept(j) {
			kept = append(kept, j)
			continue
		}
		if l.refused[j.RequestID] {
			continue
		}
		if len(l.refused) >= 1000 {
			clear(l.refused)
		}
		l.refused[j.RequestID] = true
		if l.Refused != nil {
			l.Refused(j)
		}
	}
	return kept
}

func (l *listener) open(ctx context.Context) error {
	s, err := l.Worker.OpenSession(ctx, l.ScaleSet.Repo, l.ScaleSetID, l.Owner)
	if err != nil {
		return err
	}
	l.session, l.opened, l.refreshAt = s, l.Now(), l.Now().Add(RefreshAfter)
	l.recheck = true
	return nil
}

func (l *listener) refresh(ctx context.Context) error {
	s, err := l.Worker.RefreshSession(ctx, l.ScaleSet.Repo, l.ScaleSetID, l.session.ID, l.Owner)
	if worker.Is(err, worker.NotFound) {
		return l.open(ctx)
	}
	if err != nil {
		return err
	}
	l.session, l.refreshAt = s, l.Now().Add(RefreshAfter)
	return nil
}

func (l *listener) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := l.Worker.CloseSession(ctx, l.ScaleSet.Repo, l.ScaleSetID, l.session.ID); err != nil {
		l.Log.Warn("closing session failed", "repo", l.ScaleSet.Repo, "class", l.ScaleSet.Class, "err", err)
	}
}

// publish records a message's jobs and sends the scheduler one Update.
func (l *listener) publish(ctx context.Context, msg *queue.Message, stats *queue.Statistics) error {
	u := scheduler.Update{ScaleSet: l.ScaleSet, ID: l.ScaleSetID}
	if msg != nil {
		for _, j := range append(msg.Available, msg.Assigned...) {
			l.track(j)
			l.hold(j)
		}
		for _, j := range msg.Started {
			l.track(j.Job)
			delete(l.pending, j.RequestID)
			u.Started = append(u.Started, j.RunnerName)
		}
		for i, j := range msg.Completed {
			delete(l.pending, j.RequestID)
			// GitHub leaves queueTime out of JobCompleted; the listener saw it
			// when the job was offered or assigned.
			if t, ok := l.jobs[j.RequestID]; ok && j.QueueTime.IsZero() {
				msg.Completed[i].QueueTime = t
			}
			delete(l.jobs, j.RequestID)
		}
		u.Completed = msg.Completed
	}
	assigned := len(l.jobs)
	if stats != nil {
		assigned = stats.TotalAssignedJobs
	}
	u.Jobs = l.demand(assigned)
	if assigned == 0 {
		clear(l.pending)
	}
	if l.Pending != nil {
		repos := slices.Sorted(maps.Values(l.pending))
		l.Pending(slices.Compact(repos))
	}
	select {
	case l.Events <- u:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// hold records the repository of a job the scale set holds for a runner.
func (l *listener) hold(j queue.Job) {
	if name := j.FullName(l.ScaleSet.Repo); name != "" {
		l.pending[j.RequestID] = name
	}
}

func (l *listener) track(j queue.Job) {
	if _, ok := l.jobs[j.RequestID]; ok {
		return
	}
	t := j.QueueTime
	if t.IsZero() {
		t = l.Now()
	}
	l.jobs[j.RequestID] = t
}

// demand is the queue times of the scale set's assigned jobs. GitHub's count
// is the truth; jobs the listener never saw (assigned while the box was down)
// count as queued when the session opened.
func (l *listener) demand(assigned int) []time.Time {
	if assigned == 0 {
		clear(l.jobs)
		return nil
	}
	times := slices.Collect(maps.Values(l.jobs))
	for len(times) < assigned {
		times = append(times, l.opened)
	}
	slices.SortFunc(times, time.Time.Compare)
	return times[:assigned]
}

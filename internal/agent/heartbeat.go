package agent

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/status"
	"github.com/dominikzabcik/boxrunner/internal/worker"
)

const (
	heartbeatEvery = 30 * time.Second
	heartbeatJobs  = 50
	// The Worker refuses bodies over 16 KB; the margin covers headers it counts.
	heartbeatLimit = 15 * 1024
)

// heartbeat reports the box's state to the Worker's dashboard until ctx ends.
// When the classes paused on the dashboard change, it asks supervise to list
// the repositories again, so a pause takes effect within one heartbeat.
func (a *Agent) heartbeat(ctx context.Context) {
	failing := false
	var paused []string
	for {
		jobs, err := status.Recent(a.JobsLog, heartbeatJobs)
		if err != nil {
			a.Log.Warn("reading the job log", "err", err)
		}
		a.mu.Lock()
		snap := a.snapshot
		a.mu.Unlock()
		if snap.Box == "" {
			// The scheduler has not published its first look yet.
			err = nil
		} else {
			var reply worker.HeartbeatReply
			reply, err = a.Worker.SendHeartbeat(ctx, fitHeartbeat(heartbeatBody(snap, jobs), heartbeatLimit))
			if err == nil && !slices.Equal(reply.Paused, paused) {
				paused = reply.Paused
				select {
				case a.resync <- struct{}{}:
				default:
				}
			}
		}
		switch {
		case err != nil && !failing && ctx.Err() == nil:
			a.Log.Warn("heartbeat to the Worker failed; the dashboard shows the box as last seen earlier", "err", err)
			failing = true
		case err == nil && failing:
			a.Log.Info("heartbeat to the Worker works again")
			failing = false
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(a.HeartbeatEvery):
		}
	}
}

func heartbeatBody(snap status.Snapshot, jobs []status.Job) worker.Heartbeat {
	hb := worker.Heartbeat{
		Box: snap.Box, Version: snap.Version, Latest: snap.Latest, Image: snap.Image.Built,
		Disk:    worker.HeartbeatDisk{Free: max(snap.DiskFree, 0), Floor: snap.DiskFloor},
		Classes: []worker.HeartbeatClass{},
		Runners: []worker.HeartbeatRunner{},
		Jobs:    []worker.HeartbeatJob{},
	}
	if hb.Image == "" {
		hb.Image = "not built"
	}
	for _, c := range snap.Classes {
		hb.Classes = append(hb.Classes, worker.HeartbeatClass{Name: c.Name, Priority: c.Priority, Max: c.Max})
	}
	for _, r := range snap.Runners {
		hb.Runners = append(hb.Runners, worker.HeartbeatRunner{Name: r.Name, Repo: r.Repo, Class: r.Class, State: r.Phase, Since: r.Since})
	}
	for _, j := range jobs {
		name := j.Name
		if name == "" {
			name = "job"
		}
		hb.Jobs = append(hb.Jobs, worker.HeartbeatJob{
			Runner: j.Runner, Repo: j.Repo, Class: j.Class, Name: name, Result: j.Result,
			Queued: optional(j.Queued), Started: optional(j.Started), Finished: j.Finished,
		})
	}
	return hb
}

func optional(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// fitHeartbeat leaves out the oldest jobs until the body fits in limit bytes.
// status.Recent lists jobs oldest first.
func fitHeartbeat(hb worker.Heartbeat, limit int) worker.Heartbeat {
	for len(hb.Jobs) > 0 {
		body, _ := json.Marshal(hb)
		if len(body) <= limit {
			break
		}
		hb.Jobs = hb.Jobs[1:]
	}
	return hb
}

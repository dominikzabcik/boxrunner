package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"slices"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/scheduler"
	"github.com/dominikzabcik/boxrunner/internal/seen"
	"github.com/dominikzabcik/boxrunner/internal/status"
	"github.com/dominikzabcik/boxrunner/internal/worker"
)

const (
	heartbeatEvery = 30 * time.Second
	// While the box shares a class with other boxes, a waiting job reaches the
	// box that runs it within one heartbeat, so they come more often.
	fleetHeartbeatEvery = 10 * time.Second
	// A box that has not heard the Worker this many times in a row forgets its
	// fleet shares and decides alone again.
	fleetMisses = 2
	// A new job on a scale set the box listens to sends a heartbeat at once,
	// but not more often than this.
	beatAtMost    = 2 * time.Second
	heartbeatJobs = 50
	// The Worker refuses a heartbeat with more scale sets than this.
	heartbeatScaleSets = 128
	// The Worker refuses an image longer than this.
	heartbeatImage = 128
	// The Worker refuses bodies over 16 KB; the margin covers headers it counts.
	heartbeatLimit = 15 * 1024
	// The heartbeat names the newest destinations: this many of each
	// repository and this many in all.
	heartbeatDestinationsPerRepo = 10
	heartbeatDestinations        = 40
)

// heartbeat reports the box's state to the Worker's dashboard until ctx ends.
// When the classes paused on the dashboard change, it asks supervise to list
// the repositories again, so a pause takes effect within one heartbeat.
func (a *Agent) heartbeat(ctx context.Context) {
	failing := false
	var paused []string
	var limits map[string]int
	fleet, misses := false, 0
	for {
		jobs, err := status.Recent(a.JobsLog, heartbeatJobs)
		if err != nil {
			a.Log.Warn("reading the job log", "err", err)
		}
		snap := a.current()
		if snap.Box == "" {
			// The scheduler has not published its first look yet.
			err = nil
		} else {
			var reply worker.HeartbeatReply
			hb := heartbeatBody(snap, jobs)
			hb.Rank = a.Config.Rank
			hb.Destinations = destinationsFor(a.dest.book.Repos(heartbeatDestinationsPerRepo))
			for i, ss := range hb.ScaleSets {
				hb.ScaleSets[i].Pending = a.pendingRepos(scheduler.Key{Repo: ss.Repo, Class: ss.Class})
			}
			reply, err = a.Worker.SendHeartbeat(ctx, fitHeartbeat(hb, heartbeatLimit))
			if err == nil {
				a.heartbeatOK.Store(time.Now().UnixNano())
				misses = 0
				// Before the fleet's shares, so a share never starts a runner the limits forbid.
				if !maps.Equal(reply.Limits, limits) {
					limits = reply.Limits
					a.Log.Info("limits set on the dashboard", "limits", limits)
					a.post(scheduler.Limits{Max: limits})
				}
				if reply.Fleet != nil || fleet {
					a.post(scheduler.Fleet{Shares: a.fleetShares(reply.Fleet)})
				}
				fleet = reply.Fleet != nil
			} else if misses++; fleet && misses >= fleetMisses {
				a.post(scheduler.Fleet{})
				fleet = false
			}
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
		every := a.HeartbeatEvery
		if fleet {
			every = min(every, fleetHeartbeatEvery)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		case <-a.beat:
			select {
			case <-ctx.Done():
				return
			case <-time.After(beatAtMost):
			}
		}
	}
}

func heartbeatBody(snap status.Snapshot, jobs []status.Job) worker.Heartbeat {
	hb := worker.Heartbeat{
		Box: snap.Box, Version: snap.Version, Latest: snap.Latest, Image: snap.Image.Built,
		Disk:     worker.HeartbeatDisk{Free: max(snap.DiskFree, 0), Floor: snap.DiskFloor},
		Classes:  []worker.HeartbeatClass{},
		Runners:  []worker.HeartbeatRunner{},
		Jobs:     []worker.HeartbeatJob{},
		Draining: snap.Draining,
	}
	// The Worker shows the image as text of up to 128 characters, so the
	// digest and the build in progress ride along without a new field. The
	// digest gives way when all of it would not fit.
	img := snap.Image
	switch {
	case img.Building != "" && (img.Built == "" || img.Built == img.Building):
		hb.Image = "building " + img.Building
	case hb.Image == "":
		hb.Image = "not built"
	default:
		building := ""
		if img.Building != "" {
			building = ", building " + img.Building
		}
		if from := " from " + img.Digest; img.Digest != "" && len(hb.Image+from+building) <= heartbeatImage {
			hb.Image += from
		}
		hb.Image += building
	}
	for _, c := range snap.Classes {
		hb.Classes = append(hb.Classes, worker.HeartbeatClass{Name: c.Name, Priority: c.Priority, Max: c.Max, HeldUntil: c.HeldUntil})
	}
	// Scale sets with assigned jobs come first, so the cap never drops one;
	// the others tell the Worker that their jobs no longer wait.
	scaleSets := slices.Clone(snap.ScaleSets)
	slices.SortStableFunc(scaleSets, func(x, y status.ScaleSet) int { return cmp.Compare(min(y.Assigned, 1), min(x.Assigned, 1)) })
	for _, ss := range scaleSets[:min(len(scaleSets), heartbeatScaleSets)] {
		hb.ScaleSets = append(hb.ScaleSets, worker.HeartbeatScaleSet{Repo: ss.Repo, Class: ss.Class, Assigned: ss.Assigned})
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
			Queued: optional(j.Queued), Started: optional(j.Started), Finished: j.Finished, Usage: j.Usage,
		})
	}
	return hb
}

// fleetShares turns the Worker's shares into the scheduler's, and keeps the
// repositories of an organization scale set's waiting jobs for the runners the
// box mints for them.
func (a *Agent) fleetShares(shares *[]worker.FleetShare) map[scheduler.Key]int {
	if shares == nil {
		return nil
	}
	out := map[scheduler.Key]int{}
	for _, s := range *shares {
		key := scheduler.Key{Repo: s.Repo, Class: s.Class}
		out[key] = s.Jobs
		if len(s.Repos) > 0 {
			a.setPending(key, s.Repos)
		}
	}
	return out
}

// destinationsFor lists the newest destinations of all repositories, newest
// first, leaving out those of containers that are not the agent's runners.
func destinationsFor(repos []seen.Repo) []worker.HeartbeatDestination {
	var out []worker.HeartbeatDestination
	for _, r := range repos {
		if r.Repo == "" {
			// The Worker lists destinations by repository.
			continue
		}
		for _, d := range r.Destinations {
			out = append(out, worker.HeartbeatDestination{Repo: r.Repo, Addr: d.Addr, Port: d.Port, Proto: d.Proto, Name: d.Name, First: d.First, Count: d.Count})
		}
	}
	slices.SortStableFunc(out, func(x, y worker.HeartbeatDestination) int { return y.First.Compare(x.First) })
	return out[:min(len(out), heartbeatDestinations)]
}

func optional(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// fitHeartbeat leaves out the oldest destinations, then the oldest jobs,
// until the body fits in limit bytes. status.Recent lists jobs oldest first.
func fitHeartbeat(hb worker.Heartbeat, limit int) worker.Heartbeat {
	for len(hb.Destinations) > 0 || len(hb.Jobs) > 0 {
		body, _ := json.Marshal(hb)
		if len(body) <= limit {
			break
		}
		if len(hb.Destinations) > 0 {
			hb.Destinations = hb.Destinations[:len(hb.Destinations)-1]
		} else {
			hb.Jobs = hb.Jobs[1:]
		}
	}
	return hb
}

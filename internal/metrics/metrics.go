// Package metrics writes the agent's status snapshot as Prometheus metrics in
// the text exposition format 0.0.4. docs/config.md#metrics lists them and says
// why three take actions-runner-controller's names and labels.
package metrics

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/scheduler"
	"github.com/dominikzabcik/boxrunner/internal/status"
)

// Write writes the snapshot's metrics.
func Write(w io.Writer, s status.Snapshot) error {
	var b text
	b.family("boxrunner_agent_info", "gauge", "The agent's version and box name.")
	b.sample("boxrunner_agent_info", 1, "box", s.Box, "version", s.Version)
	if s.Worker != "" {
		b.family("boxrunner_worker_info", "gauge", "The Worker's version, once it answered.")
		b.sample("boxrunner_worker_info", 1, "version", s.Worker)
	}
	if s.Image.Built != "" {
		b.family("boxrunner_runner_image_info", "gauge", "The Actions runner version the job image was built with.")
		b.sample("boxrunner_runner_image_info", 1, "version", s.Image.Built)
	}
	if s.Image.Building != "" {
		b.family("boxrunner_runner_image_building_info", "gauge", "The job image the agent is building, or whose class images it is building, while it builds.")
		b.sample("boxrunner_runner_image_building_info", 1, "version", s.Image.Building)
	}

	b.family("boxrunner_class_runners", "gauge", "Runners of the class in each phase.")
	for _, c := range s.Classes {
		phases := map[string]int{}
		for _, r := range s.Runners {
			if r.Class == c.Name {
				phases[r.Phase]++
			}
		}
		for p := scheduler.Requested; p <= scheduler.Exited; p++ {
			b.sample("boxrunner_class_runners", float64(phases[p.String()]), "class", c.Name, "phase", p.String())
		}
	}
	b.family("boxrunner_class_max_runners", "gauge", "Runners of the class the box starts or runs at once, for all its repositories together.")
	for _, c := range s.Classes {
		b.sample("boxrunner_class_max_runners", float64(c.Max), "class", c.Name)
	}
	b.family("boxrunner_class_held", "gauge", "1 while the class holds its starts because its runners exit before taking a job.")
	for _, c := range s.Classes {
		b.sample("boxrunner_class_held", flag(!c.HeldUntil.IsZero()), "class", c.Name)
	}

	if len(s.Egress) > 0 {
		b.family("boxrunner_egress_ready", "gauge", "1 while the agent serves the class with its egress allowlist in force, 0 when it does not.")
		for _, e := range s.Egress {
			b.sample("boxrunner_egress_ready", flag(e.Problem == ""), "class", e.Class)
		}
		b.family("boxrunner_egress_blocked_total", "counter", "Packets dropped and lookups refused by the class's egress allowlist since the agent started.")
		for _, e := range s.Egress {
			b.sample("boxrunner_egress_blocked_total", float64(e.Packets), "class", e.Class, "kind", "packet")
			b.sample("boxrunner_egress_blocked_total", float64(e.Lookups), "class", e.Class, "kind", "lookup")
		}
	}

	b.family("gha_assigned_jobs", "gauge", "Number of jobs assigned to this scale set.")
	for _, ss := range s.ScaleSets {
		b.sample("gha_assigned_jobs", float64(ss.Assigned), scaleSet(ss.Repo, ss.Class, "")...)
	}
	if slices.ContainsFunc(s.ScaleSets, func(ss status.ScaleSet) bool { return len(ss.Refused) > 0 }) {
		b.family("boxrunner_refused_jobs_total", "counter", "Jobs an organization scale set was offered from repositories this box does not serve, by repository, since the agent started. They wait at GitHub.")
		for _, ss := range s.ScaleSets {
			for _, repo := range slices.Sorted(maps.Keys(ss.Refused)) {
				b.sample("boxrunner_refused_jobs_total", float64(ss.Refused[repo]), scaleSet(repo, ss.Class, "")...)
			}
		}
	}
	b.family("gha_completed_jobs_total", "counter", "Total number of jobs completed since the agent started.")
	for _, t := range s.Finished {
		b.sample("gha_completed_jobs_total", float64(t.Jobs), scaleSet(t.Repo, t.Class, t.Result)...)
	}
	b.family("gha_job_execution_duration_seconds", "histogram", "Time spent executing workflow jobs by the scale set (in seconds).")
	for _, t := range s.Finished {
		b.histogram("gha_job_execution_duration_seconds", t.Took, scaleSet(t.Repo, t.Class, t.Result))
	}
	b.family("boxrunner_job_queue_duration_seconds", "histogram", "Time from when a job was queued to when a runner took it, counted when the job finishes.")
	for _, t := range s.Finished {
		b.histogram("boxrunner_job_queue_duration_seconds", t.Wait, scaleSet(t.Repo, t.Class, t.Result))
	}

	b.family("boxrunner_disk_free_bytes", "gauge", "Free space on Docker's or Sysbox's filesystem, whichever has less.")
	b.sample("boxrunner_disk_free_bytes", float64(s.DiskFree))
	b.family("boxrunner_disk_floor_bytes", "gauge", "Free space below which no runner starts.")
	b.sample("boxrunner_disk_floor_bytes", float64(s.DiskFloor))
	if s.Image.Disk > 0 {
		b.family("boxrunner_images_bytes", "gauge", "Space the job, class and runner images take, measured after the last image prune.")
		b.sample("boxrunner_images_bytes", float64(s.Image.Disk))
	}
	b.family("boxrunner_draining", "gauge", "1 while the box drains and starts no runner.")
	b.sample("boxrunner_draining", flag(s.Draining))
	b.family("boxrunner_heartbeat_last_success_timestamp_seconds", "gauge", "When the Worker last took a heartbeat, in seconds since 1970; 0 before the first.")
	heartbeat := 0.0
	if !s.Heartbeat.IsZero() {
		heartbeat = float64(s.Heartbeat.UnixMilli()) / 1000
	}
	b.sample("boxrunner_heartbeat_last_success_timestamp_seconds", heartbeat)

	_, err := w.Write(b.Bytes())
	return err
}

// scaleSet is the labels actions-runner-controller gives a scale set's
// metrics, with job_result when result is set.
func scaleSet(repo, class, result string) []string {
	owner, name, _ := strings.Cut(repo, "/")
	labels := []string{"name", class, "organization", owner, "repository", name}
	if result != "" {
		labels = append(labels, "job_result", result)
	}
	return labels
}

func flag(on bool) float64 {
	if on {
		return 1
	}
	return 0
}

type text struct{ bytes.Buffer }

func (b *text) family(name, kind, help string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
}

// sample writes one line; labels are name and value pairs.
func (b *text) sample(name string, value float64, labels ...string) {
	b.WriteString(name)
	for i := 0; i < len(labels); i += 2 {
		sep := ","
		if i == 0 {
			sep = "{"
		}
		b.WriteString(sep + labels[i] + `="` + escape(labels[i+1]) + `"`)
	}
	if len(labels) > 0 {
		b.WriteByte('}')
	}
	b.WriteString(" " + strconv.FormatFloat(value, 'f', -1, 64) + "\n")
}

func (b *text) histogram(name string, h status.Histogram, labels []string) {
	var count uint64
	for i, n := range h.Counts {
		count += n
		le := "+Inf"
		if i < len(status.Buckets) {
			le = strconv.FormatFloat(status.Buckets[i], 'f', -1, 64)
		}
		b.sample(name+"_bucket", float64(count), append(labels[:len(labels):len(labels)], "le", le)...)
	}
	b.sample(name+"_sum", h.Sum, labels...)
	b.sample(name+"_count", float64(count), labels...)
}

var escape = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace

// Serve answers GET /metrics on l until ctx ends, calling snapshot for every
// request.
func Serve(ctx context.Context, l net.Listener, snapshot func() status.Snapshot) error {
	srv := &http.Server{Handler: handler(snapshot), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func handler(snapshot func() status.Snapshot) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "use GET", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		Write(w, snapshot())
	})
}

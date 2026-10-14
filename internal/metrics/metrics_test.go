package metrics

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/status"
)

var t0 = time.Date(2026, 10, 13, 21, 0, 0, 0, time.UTC)

// snapshot is a box with two classes: one running a job and holding a warm
// runner, the other held after its runners crashed. Two jobs have finished.
func snapshot() status.Snapshot {
	finished := status.Count(nil, status.Job{Repo: "octo/site", Class: "boxrunner", Result: "succeeded",
		Queued: t0, Started: t0.Add(8 * time.Second), Finished: t0.Add(188 * time.Second)})
	finished = status.Count(finished, status.Job{Repo: "octo/docs", Class: "boxrunner-light", Result: "failed",
		Started: t0, Finished: t0.Add(45 * time.Second)})
	return status.Snapshot{
		Box: "box-1", Version: "0.4.0", Worker: "0.4.0",
		Classes: []status.Class{
			{Name: "boxrunner", Priority: "normal", Max: 2, Active: 2},
			{Name: "boxrunner-light", Priority: "idle", Max: 1, HeldUntil: t0.Add(time.Minute)},
		},
		ScaleSets: []status.ScaleSet{{Repo: "octo/docs", Class: "boxrunner-light", Assigned: 0}, {Repo: "octo/site", Class: "boxrunner", Assigned: 1}},
		Runners: []status.Runner{
			{Name: "box-1-boxrunner-a1b2c3", Repo: "octo/site", Class: "boxrunner", Phase: "running", Since: t0},
			{Name: "box-1-boxrunner-d4e5f6", Repo: "octo/site", Class: "boxrunner", Phase: "starting", Since: t0, Warm: true},
		},
		Image:     status.ImageStatus{Built: "2.330.0", Latest: "2.330.0", Disk: 2_980_000_000},
		DiskFree:  120_000_000_000,
		DiskFloor: 10_000_000_000,
		Draining:  true,
		Finished:  finished,
		Heartbeat: time.Date(2026, 10, 13, 21, 4, 30, 250_000_000, time.UTC),
	}
}

func TestWrite(t *testing.T) {
	want, err := os.ReadFile("testdata/metrics.txt")
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if err := Write(&got, snapshot()); err != nil {
		t.Fatal(err)
	}
	if got.String() != string(want) {
		t.Errorf("Write =\n%s\nwant testdata/metrics.txt:\n%s", got.String(), want)
	}
}

func TestWriteBeforeTheFirstLook(t *testing.T) {
	want := `# HELP boxrunner_agent_info The agent's version and box name.
# TYPE boxrunner_agent_info gauge
boxrunner_agent_info{box="box-1",version="0.4.0"} 1
# HELP boxrunner_class_runners Runners of the class in each phase.
# TYPE boxrunner_class_runners gauge
# HELP boxrunner_class_max_runners Runners of the class the box starts or runs at once, for all its repositories together.
# TYPE boxrunner_class_max_runners gauge
# HELP boxrunner_class_held 1 while the class holds its starts because its runners exit before taking a job.
# TYPE boxrunner_class_held gauge
# HELP gha_assigned_jobs Number of jobs assigned to this scale set.
# TYPE gha_assigned_jobs gauge
# HELP gha_completed_jobs_total Total number of jobs completed since the agent started.
# TYPE gha_completed_jobs_total counter
# HELP gha_job_execution_duration_seconds Time spent executing workflow jobs by the scale set (in seconds).
# TYPE gha_job_execution_duration_seconds histogram
# HELP boxrunner_job_queue_duration_seconds Time from when a job was queued to when a runner took it, counted when the job finishes.
# TYPE boxrunner_job_queue_duration_seconds histogram
# HELP boxrunner_disk_free_bytes Free space on Docker's or Sysbox's filesystem, whichever has less.
# TYPE boxrunner_disk_free_bytes gauge
boxrunner_disk_free_bytes 0
# HELP boxrunner_disk_floor_bytes Free space below which no runner starts.
# TYPE boxrunner_disk_floor_bytes gauge
boxrunner_disk_floor_bytes 0
# HELP boxrunner_draining 1 while the box drains and starts no runner.
# TYPE boxrunner_draining gauge
boxrunner_draining 0
# HELP boxrunner_heartbeat_last_success_timestamp_seconds When the Worker last took a heartbeat, in seconds since 1970; 0 before the first.
# TYPE boxrunner_heartbeat_last_success_timestamp_seconds gauge
boxrunner_heartbeat_last_success_timestamp_seconds 0
`
	var got bytes.Buffer
	Write(&got, status.Snapshot{Box: "box-1", Version: "0.4.0"})
	if got.String() != want {
		t.Errorf("Write =\n%s\nwant\n%s", got.String(), want)
	}
}

func TestEscapesLabelValues(t *testing.T) {
	var got bytes.Buffer
	Write(&got, status.Snapshot{Box: "box-1", Version: "0.4.0 \"dev\"\\\n"})
	want := `boxrunner_agent_info{box="box-1",version="0.4.0 \"dev\"\\\n"} 1` + "\n"
	if !bytes.Contains(got.Bytes(), []byte(want)) {
		t.Errorf("Write =\n%s\nwant a line\n%s", got.String(), want)
	}
}

func TestServe(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, l, snapshot) }()
	base := "http://" + l.Addr().String()

	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	want, _ := os.ReadFile("testdata/metrics.txt")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" || string(body) != string(want) {
		t.Errorf("GET /metrics = %d %q\n%s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	for _, c := range []struct {
		method, path string
		code         int
	}{
		{"POST", "/metrics", 405},
		{"GET", "/", 404},
		{"GET", "/status", 404},
	} {
		req, _ := http.NewRequest(c.method, base+c.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.code {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, resp.StatusCode, c.code)
		}
	}

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after ctx ended")
	}
	if _, err := http.Get(base + "/metrics"); err == nil {
		t.Error("the listener still answers after Serve returned")
	}
}

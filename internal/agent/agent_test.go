package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/dominikzabcik/boxrunner/internal/runnerimage"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/config"
	"github.com/dominikzabcik/boxrunner/internal/docker"
	"github.com/dominikzabcik/boxrunner/internal/scheduler"
	"github.com/dominikzabcik/boxrunner/internal/status"
	"github.com/dominikzabcik/boxrunner/internal/worker"
)

type container struct {
	Name    string
	Labels  map[string]string
	State   string
	Created int64
	Body    map[string]any
}

// fakeDocker keeps containers in memory and serves the Engine API calls the
// agent makes on a unix socket.
type fakeDocker struct {
	mu         sync.Mutex
	containers map[string]*container
	network    bool
	runtimes   []string
}

func (f *fakeDocker) serve(t *testing.T) string {
	socket := filepath.Join(t.TempDir(), "d.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/v1.47")
		parts := strings.Split(strings.Trim(path, "/"), "/")
		switch {
		case path == "/version":
			io.WriteString(w, `{"ApiVersion":"1.47"}`)
		case path == "/info":
			runtimes := map[string]any{"runc": map[string]string{"path": "runc"}}
			for _, r := range f.runtimes {
				runtimes[r] = map[string]string{"path": "/usr/bin/" + r}
			}
			json.NewEncoder(w).Encode(map[string]any{"DockerRootDir": root, "CgroupDriver": "systemd", "CgroupVersion": "2", "Runtimes": runtimes})
		case path == "/networks/boxrunner":
			if !f.network {
				http.Error(w, `{"message":"no such network"}`, 404)
				return
			}
			io.WriteString(w, `{"Name":"boxrunner"}`)
		case path == "/networks/create":
			f.network = true
		case strings.HasPrefix(path, "/images/"):
			io.WriteString(w, `{"Config":{"Labels":{"boxrunner.image":"`+runnerimage.Identity("2.338.0")+`"}}}`)
		case path == "/containers/json":
			var list []map[string]any
			for _, c := range f.containers {
				list = append(list, map[string]any{"Id": c.Name, "Names": []string{"/" + c.Name}, "Labels": c.Labels, "State": c.State, "Created": c.Created})
			}
			json.NewEncoder(w).Encode(list)
		case path == "/containers/create":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			labels := map[string]string{}
			for k, v := range body["Labels"].(map[string]any) {
				labels[k] = v.(string)
			}
			name := r.URL.Query().Get("name")
			f.containers[name] = &container{Name: name, Labels: labels, State: "created", Created: time.Now().Unix(), Body: body}
			fmt.Fprintf(w, `{"Id":%q}`, name)
		case len(parts) == 3 && parts[2] == "start":
			f.containers[parts[1]].State = "running"
			w.WriteHeader(204)
		case r.Method == http.MethodDelete && parts[0] == "containers":
			if _, ok := f.containers[parts[1]]; !ok {
				http.Error(w, `{"message":"no such container"}`, 404)
				return
			}
			delete(f.containers, parts[1])
			w.WriteHeader(204)
		default:
			t.Errorf("fake docker: unexpected %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", 500)
		}
	}))
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	return socket
}

func (f *fakeDocker) snapshot() map[string]container {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]container{}
	for k, v := range f.containers {
		out[k] = *v
	}
	return out
}

// fakeGitHub plays the Worker and the scale set's queue. Messages put on
// queue are served to the long poll in order.
type fakeGitHub struct {
	mu    sync.Mutex
	calls []string
	queue chan string
	// busy are runner ids that took a job, so removing them answers 409.
	busy map[string]bool
}

func (g *fakeGitHub) serve(t *testing.T) *httptest.Server {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			g.mu.Lock()
			g.calls = append(g.calls, strings.TrimSpace(fmt.Sprintf("%s %s %s", r.Method, r.URL.RequestURI(), body)))
			g.mu.Unlock()
			w.Header().Set("Boxrunner-Protocol", "1")
		}
		session := fmt.Sprintf(`{"sessionId":"s1","queueUrl":"%s/queue/7","queueToken":"q1","acquireJobsUrl":"%s/acquire","statistics":{"totalAssignedJobs":0}}`, srv.URL, srv.URL)
		switch {
		case r.URL.Path == "/v1/repos":
			io.WriteString(w, `{"repos":[{"name":"octo/site","id":123}],"classes":["boxrunner","boxrunner-light"]}`)
		case r.URL.Path == "/v1/scalesets":
			io.WriteString(w, `{"id":7,"name":"boxrunner"}`)
		case r.URL.Path == "/v1/sessions":
			io.WriteString(w, session)
		case strings.HasPrefix(r.URL.Path, "/v1/sessions/"):
			w.WriteHeader(204)
		case r.URL.Path == "/v1/runners":
			io.WriteString(w, `{"runnerId":41,"jitConfig":"jit-41"}`)
		case strings.HasPrefix(r.URL.Path, "/v1/runners/"):
			if g.busy[strings.TrimPrefix(r.URL.Path, "/v1/runners/")] {
				w.WriteHeader(409)
				io.WriteString(w, `{"error":"runner_busy","message":"took a job"}`)
				return
			}
			w.WriteHeader(204)
		case r.URL.Path == "/queue/7" && r.Method == http.MethodGet:
			select {
			case m := <-g.queue:
				io.WriteString(w, m)
			case <-time.After(200 * time.Millisecond):
				w.WriteHeader(202)
			case <-r.Context().Done():
			}
		case strings.HasPrefix(r.URL.Path, "/queue/7/"):
			w.WriteHeader(204)
		case r.URL.Path == "/acquire":
			fmt.Fprintf(w, `{"count":1,"value":%s}`, body)
		case r.URL.Path == "/releases/latest":
			http.Redirect(w, r, "/releases/tag/v2.338.0", http.StatusFound)
		default:
			t.Errorf("fake github: unexpected %s %s", r.Method, r.URL)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (g *fakeGitHub) workerCalls() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.calls...)
}

func message(id int, assigned int, jobs ...string) string {
	body, _ := json.Marshal("[" + strings.Join(jobs, ",") + "]")
	return fmt.Sprintf(`{"messageId":%d,"messageType":"RunnerScaleSetJobMessages","statistics":{"totalAssignedJobs":%d},"body":%s}`, id, assigned, body)
}

type harness struct {
	docker *fakeDocker
	github *fakeGitHub
	jobs   string
	cancel context.CancelFunc
	done   chan error
}

// startAgent runs the agent with one class; change may adjust the class and
// the fake Docker before the agent starts.
func startAgent(t *testing.T, containers map[string]*container, busy map[string]bool, change ...func(*config.Class, *fakeDocker)) *harness {
	h := &harness{
		docker: &fakeDocker{containers: containers},
		github: &fakeGitHub{queue: make(chan string, 8), busy: busy},
		jobs:   filepath.Join(t.TempDir(), "jobs.jsonl"),
		done:   make(chan error, 1),
	}
	cfg := config.Config{
		Box: "box-1", DiskFloor: 1,
		Classes: []config.Class{{Name: "boxrunner", NanoCPUs: 2e9, Memory: 4 << 30, Pids: 4096, Shm: 1 << 30, Priority: config.Normal, Max: 1, Image: "boxrunner/runner:local"}},
	}
	for _, f := range change {
		f(&cfg.Classes[0], h.docker)
	}
	socket := h.docker.serve(t)
	srv := h.github.serve(t)
	d, err := docker.Dial(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(srv.URL)
	cfg.Worker = u
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	var ctx context.Context
	ctx, h.cancel = context.WithCancel(context.Background())
	t.Cleanup(h.cancel)
	go func() {
		h.done <- Run(ctx, Options{
			Config: cfg, Docker: d, Version: "0.1.0", Log: log,
			Worker:      worker.New(worker.Options{URL: u, Token: "box-token", Version: "0.1.0", Log: log}),
			Run:         func(context.Context, string, string, ...string) (string, error) { return "", nil },
			JobsLog:     h.jobs,
			Socket:      filepath.Join(t.TempDir(), "s.sock"),
			ReleasesURL: srv.URL + "/releases/latest",
			Tick:        50 * time.Millisecond,
		})
	}()
	return h
}

func (h *harness) stop(t *testing.T) {
	h.cancel()
	select {
	case err := <-h.done:
		if err != nil {
			t.Errorf("Run = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the agent did not stop")
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestJobLifecycle(t *testing.T) {
	h := startAgent(t, map[string]*container{}, nil)
	h.github.queue <- message(1, 1, `{"messageType":"JobAvailable","runnerRequestId":501,"queueTime":"2026-10-13T21:00:00Z"}`)

	var name string
	waitFor(t, "a running job container", func() bool {
		for n, c := range h.docker.snapshot() {
			name = n
			return c.State == "running"
		}
		return false
	})
	c := h.docker.snapshot()[name]
	var want map[string]any
	json.Unmarshal([]byte(fmt.Sprintf(`{
		"Image": "boxrunner/runner:local",
		"Env": ["ACTIONS_RUNNER_INPUT_JITCONFIG=jit-41"],
		"Labels": {"boxrunner.box": "box-1", "boxrunner.runner": %q, "boxrunner.runner-id": "41",
			"boxrunner.repo": "octo/site", "boxrunner.class": "boxrunner", "boxrunner.scaleset-id": "7"},
		"HostConfig": {"NanoCpus": 2000000000, "Memory": 4294967296, "MemorySwap": 4294967296,
			"PidsLimit": 4096, "ShmSize": 1073741824, "NetworkMode": "boxrunner"}
	}`, name)), &want)
	if !reflect.DeepEqual(c.Body, want) {
		t.Errorf("container create body =\n%v\nwant\n%v", c.Body, want)
	}
	if !strings.HasPrefix(name, "box-1-boxrunner-") || len(name) != len("box-1-boxrunner-")+6 {
		t.Errorf("runner name %q", name)
	}

	h.github.queue <- message(2, 1, fmt.Sprintf(`{"messageType":"JobStarted","runnerRequestId":501,"runnerId":41,"runnerName":%q}`, name))
	h.github.queue <- message(3, 0, fmt.Sprintf(`{"messageType":"JobCompleted","runnerRequestId":501,"runnerId":41,"runnerName":%q,"result":"succeeded","jobDisplayName":"test",
		"queueTime":"2026-10-13T21:00:00Z","runnerAssignTime":"2026-10-13T21:00:08Z","finishTime":"2026-10-13T21:03:00Z"}`, name))
	waitFor(t, "the finished job's container to go", func() bool { return len(h.docker.snapshot()) == 0 })
	h.stop(t)

	wantCalls := []string{
		"GET /v1/repos",
		`POST /v1/scalesets {"class":"boxrunner","repo":"octo/site"}`,
		`POST /v1/sessions {"owner":"box-1","repo":"octo/site","scaleSetId":7}`,
		fmt.Sprintf(`POST /v1/runners {"name":%q,"repo":"octo/site","scaleSetId":7}`, name),
		"DELETE /v1/sessions/s1?repo=octo%2Fsite&scaleSetId=7",
	}
	if got := h.github.workerCalls(); !reflect.DeepEqual(got, wantCalls) {
		t.Errorf("worker calls =\n%q\nwant\n%q", got, wantCalls)
	}
	jobs, _ := status.Recent(h.jobs, 10)
	wantJob := status.Job{Repo: "octo/site", Class: "boxrunner", Runner: name, Name: "test", Result: "succeeded",
		Queued: time.Date(2026, 10, 13, 21, 0, 0, 0, time.UTC), Started: time.Date(2026, 10, 13, 21, 0, 8, 0, time.UTC), Finished: time.Date(2026, 10, 13, 21, 3, 0, 0, time.UTC)}
	if len(jobs) != 1 || !reflect.DeepEqual(jobs[0], wantJob) {
		t.Errorf("job log = %+v", jobs)
	}
}

func TestStopRemovesRunnersWithoutAJob(t *testing.T) {
	h := startAgent(t, map[string]*container{}, nil)
	h.github.queue <- message(1, 1, `{"messageType":"JobAvailable","runnerRequestId":501}`)
	waitFor(t, "a running job container", func() bool {
		for _, c := range h.docker.snapshot() {
			return c.State == "running"
		}
		return false
	})
	h.stop(t)
	if left := h.docker.snapshot(); len(left) != 0 {
		t.Errorf("containers left after stop: %v", left)
	}
	calls := h.github.workerCalls()
	if !slices.Contains(calls, "DELETE /v1/runners/41?repo=octo%2Fsite&scaleSetId=7") || !slices.Contains(calls, "DELETE /v1/sessions/s1?repo=octo%2Fsite&scaleSetId=7") {
		t.Errorf("worker calls = %q", calls)
	}
}

func TestRestartAdoptsContainers(t *testing.T) {
	old := time.Now().Add(-5 * time.Minute).Unix()
	labels := func(name, id string) map[string]string {
		return map[string]string{"boxrunner.box": "box-1", "boxrunner.runner": name, "boxrunner.runner-id": id,
			"boxrunner.repo": "octo/site", "boxrunner.class": "boxrunner", "boxrunner.scaleset-id": "7"}
	}
	h := startAgent(t, map[string]*container{
		"box-1-boxrunner-aaaaaa": {Name: "box-1-boxrunner-aaaaaa", Labels: labels("box-1-boxrunner-aaaaaa", "40"), State: "running", Created: old},
		"box-1-boxrunner-bbbbbb": {Name: "box-1-boxrunner-bbbbbb", Labels: labels("box-1-boxrunner-bbbbbb", "42"), State: "running", Created: old},
		"box-1-boxrunner-cccccc": {Name: "box-1-boxrunner-cccccc", Labels: labels("box-1-boxrunner-cccccc", "43"), State: "exited", Created: old},
	}, map[string]bool{"40": true})
	waitFor(t, "the idle and exited containers to go", func() bool { return len(h.docker.snapshot()) == 1 })
	time.Sleep(300 * time.Millisecond)
	h.stop(t)

	if left := h.docker.snapshot(); len(left) != 1 || left["box-1-boxrunner-aaaaaa"].State != "running" {
		t.Errorf("containers left = %v; want only the busy one", left)
	}
	var removals []string
	for _, c := range h.github.workerCalls() {
		if strings.HasPrefix(c, "DELETE /v1/runners/") {
			removals = append(removals, c)
		}
	}
	slices.Sort(removals)
	want := []string{
		"DELETE /v1/runners/40?repo=octo%2Fsite&scaleSetId=7",
		"DELETE /v1/runners/42?repo=octo%2Fsite&scaleSetId=7",
		"DELETE /v1/runners/43?repo=octo%2Fsite&scaleSetId=7",
	}
	if !reflect.DeepEqual(removals, want) {
		t.Errorf("runner removals = %q", removals)
	}
}

func TestSpecIdleClass(t *testing.T) {
	class := config.Class{Name: "boxrunner-light", NanoCPUs: 4e9, Memory: 2 << 30, Pids: 512, Shm: 1 << 30, Priority: config.Idle, Image: "boxrunner/runner:local"}
	r := scheduler.Runner{Name: "box-1-boxrunner-light-3f9a2c", ScaleSet: scheduler.Key{Repo: "octo/site", Class: "boxrunner-light"}, ScaleSetID: 8, ID: 41}
	got := Spec("box-1", class, r, "jit")
	want := docker.ContainerSpec{
		Name: "box-1-boxrunner-light-3f9a2c", Image: "boxrunner/runner:local",
		Env: []string{"ACTIONS_RUNNER_INPUT_JITCONFIG=jit"},
		Labels: map[string]string{"boxrunner.box": "box-1", "boxrunner.runner": "box-1-boxrunner-light-3f9a2c", "boxrunner.runner-id": "41",
			"boxrunner.repo": "octo/site", "boxrunner.class": "boxrunner-light", "boxrunner.scaleset-id": "8"},
		NanoCPUs: 4e9, Memory: 2 << 30, PidsLimit: 512, ShmSize: 1 << 30,
		CgroupParent: "boxrunner_idle.slice", Network: "boxrunner",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Spec =\n%+v\nwant\n%+v", got, want)
	}
}

func TestSpecDockerClass(t *testing.T) {
	class := config.Class{Name: "boxrunner", NanoCPUs: 2e9, Memory: 4 << 30, Pids: 4096, Shm: 1 << 30, Priority: config.Normal, Image: "boxrunner/runner:local", Docker: true}
	r := scheduler.Runner{Name: "box-1-boxrunner-3f9a2c", ScaleSet: scheduler.Key{Repo: "octo/site", Class: "boxrunner"}, ScaleSetID: 7, ID: 41}
	got := Spec("box-1", class, r, "jit")
	want := docker.ContainerSpec{
		Name: "box-1-boxrunner-3f9a2c", Image: "boxrunner/runner:local",
		Env: []string{"ACTIONS_RUNNER_INPUT_JITCONFIG=jit", "BOXRUNNER_DOCKER=1"},
		Labels: map[string]string{"boxrunner.box": "box-1", "boxrunner.runner": "box-1-boxrunner-3f9a2c", "boxrunner.runner-id": "41",
			"boxrunner.repo": "octo/site", "boxrunner.class": "boxrunner", "boxrunner.scaleset-id": "7"},
		NanoCPUs: 2e9, Memory: 4 << 30, PidsLimit: 4096, ShmSize: 1 << 30,
		Network: "boxrunner", Runtime: "sysbox-runc",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Spec =\n%+v\nwant\n%+v", got, want)
	}
}

func TestDockerClassNeedsSysbox(t *testing.T) {
	without := startAgent(t, map[string]*container{}, nil, func(c *config.Class, _ *fakeDocker) { c.Docker = true })
	waitFor(t, "the repository list", func() bool { return len(without.github.workerCalls()) > 0 })
	time.Sleep(300 * time.Millisecond)
	without.stop(t)
	if got := without.github.workerCalls(); !reflect.DeepEqual(got, []string{"GET /v1/repos"}) {
		t.Errorf("without Sysbox, worker calls = %q; want only the repository list", got)
	}

	with := startAgent(t, map[string]*container{}, nil, func(c *config.Class, d *fakeDocker) {
		c.Docker = true
		d.runtimes = []string{"sysbox-runc"}
	})
	with.github.queue <- message(1, 1, `{"messageType":"JobAvailable","runnerRequestId":501}`)
	var body map[string]any
	waitFor(t, "a running job container", func() bool {
		for _, c := range with.docker.snapshot() {
			body = c.Body
			return c.State == "running"
		}
		return false
	})
	with.stop(t)
	host := body["HostConfig"].(map[string]any)
	if host["Runtime"] != "sysbox-runc" || !reflect.DeepEqual(body["Env"], []any{"ACTIONS_RUNNER_INPUT_JITCONFIG=jit-41", "BOXRUNNER_DOCKER=1"}) {
		t.Errorf("with Sysbox, runtime %v and env %v", host["Runtime"], body["Env"])
	}
}

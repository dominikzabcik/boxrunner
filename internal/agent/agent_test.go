package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
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
	"github.com/dominikzabcik/boxrunner/internal/dnsfwd"
	"github.com/dominikzabcik/boxrunner/internal/docker"
	"github.com/dominikzabcik/boxrunner/internal/egress"
	"github.com/dominikzabcik/boxrunner/internal/runnerdigests"
	"github.com/dominikzabcik/boxrunner/internal/runnerimage"
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
	ImageID string
}

// fakeDocker keeps containers in memory and serves the Engine API calls the
// agent makes on a unix socket.
type fakeDocker struct {
	mu         sync.Mutex
	containers map[string]*container
	network    bool
	volumes    map[string]bool
	runtimes   []string
	// exitOnStart makes every container exit as soon as it starts.
	exitOnStart bool
	// logs names the containers whose output was read.
	logs []string
	// images are what /images/json lists; removing one takes it out.
	images        []docker.ImageSummary
	removedImages []string
	// built is the job image's identity label; 2.338.0's when empty.
	built string
	// builds are the pulls, tags and builds the agent asked for.
	builds []string
	// holdBuild, when set, keeps every build waiting until it closes.
	holdBuild chan struct{}
	// nets are the networks of egress classes, by name.
	nets map[string]*fakeNet
	// subnet and gateway, when set, are those of every new egress network.
	subnet, gateway string
	// failInspect makes the nth inspection of a network, by name, fail.
	failInspect map[string]int
	inspected   map[string]int
	// failRemove names the containers that cannot be removed, and
	// failNetwork makes creating an egress network fail.
	failRemove  map[string]bool
	failNetwork bool
	// stopped names the containers that were stopped.
	stopped []string
}

// fakeNet is an egress class's network.
type fakeNet struct {
	Bridge string
	Labels map[string]string
	Subnet string
	// Gateway is the network's address on the host.
	Gateway string
}

// matches applies a containers/json label filter, key or key=value.
func (c *container) matches(filters string) bool {
	var f map[string][]string
	json.Unmarshal([]byte(filters), &f)
	for _, label := range f["label"] {
		key, value, withValue := strings.Cut(label, "=")
		if have, ok := c.Labels[key]; !ok || withValue && have != value {
			return false
		}
	}
	return true
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
		case path == "/networks" && r.Method == http.MethodGet:
			var filters map[string][]string
			json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters)
			list := []map[string]string{}
			for name, n := range f.nets {
				if labels := filters["label"]; len(labels) == 0 {
					list = append(list, map[string]string{"Name": name})
				} else if _, ok := n.Labels[strings.Split(labels[0], "=")[0]]; ok {
					list = append(list, map[string]string{"Name": name})
				}
			}
			if len(filters["label"]) == 0 && f.network {
				list = append(list, map[string]string{"Name": "boxrunner"})
			}
			json.NewEncoder(w).Encode(list)
		case strings.HasPrefix(path, "/networks/") && f.nets[strings.TrimPrefix(path, "/networks/")] != nil && r.Method == http.MethodGet:
			name := strings.TrimPrefix(path, "/networks/")
			n := f.nets[name]
			if f.inspected == nil {
				f.inspected = map[string]int{}
			}
			if f.inspected[name]++; f.inspected[name] == f.failInspect[name] {
				http.Error(w, `{"message":"docker is busy"}`, 500)
				return
			}
			endpoints := map[string]any{}
			for _, c := range f.containers {
				if mode, _ := c.Body["HostConfig"].(map[string]any)["NetworkMode"].(string); mode == name {
					endpoints[c.Name] = map[string]string{"Name": c.Name, "IPv4Address": "172.30.0.7/16"}
				}
			}
			json.NewEncoder(w).Encode(map[string]any{
				"Name": name, "Driver": "bridge", "Labels": n.Labels, "Containers": endpoints,
				"Options": map[string]string{"com.docker.network.bridge.name": n.Bridge},
				"IPAM":    map[string]any{"Config": []map[string]string{{"Subnet": n.Subnet, "Gateway": n.Gateway}}},
			})
		case strings.HasPrefix(path, "/networks/") && r.Method == http.MethodDelete:
			delete(f.nets, strings.TrimPrefix(path, "/networks/"))
			w.WriteHeader(204)
		case path == "/networks/boxrunner":
			if !f.network {
				http.Error(w, `{"message":"no such network"}`, 404)
				return
			}
			io.WriteString(w, `{"Name":"boxrunner","IPAM":{"Config":[{"Subnet":"172.18.0.0/16","Gateway":"172.18.0.1"}]}}`)
		case strings.HasPrefix(path, "/networks/") && r.Method == http.MethodGet:
			http.Error(w, `{"message":"no such network"}`, 404)
		case path == "/networks/create":
			var body struct {
				Name    string
				Options map[string]string
				Labels  map[string]string
				IPAM    struct {
					Config []struct{ Subnet, Gateway string }
				}
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.Name == "boxrunner" {
				f.network = true
				break
			}
			if f.failNetwork {
				http.Error(w, `{"message":"Pool overlaps with other one on this address space"}`, http.StatusForbidden)
				return
			}
			if f.nets == nil {
				f.nets = map[string]*fakeNet{}
			}
			n := &fakeNet{Bridge: body.Options["com.docker.network.bridge.name"], Labels: body.Labels,
				Subnet:  cmp.Or(f.subnet, fmt.Sprintf("172.%d.0.0/16", 30+len(f.nets))),
				Gateway: cmp.Or(f.gateway, fmt.Sprintf("172.%d.0.1", 30+len(f.nets)))}
			if len(body.IPAM.Config) == 1 {
				n.Subnet, n.Gateway = body.IPAM.Config[0].Subnet, body.IPAM.Config[0].Gateway
			}
			f.nets[body.Name] = n
		case path == "/volumes/create":
			var body struct{ Name string }
			json.NewDecoder(r.Body).Decode(&body)
			f.volumes[body.Name] = true
		case parts[0] == "volumes" && len(parts) == 2:
			if !f.volumes[parts[1]] {
				http.Error(w, `{"message":"no such volume"}`, 404)
				return
			}
			io.WriteString(w, `{}`)
		case path == "/images/json":
			json.NewEncoder(w).Encode(f.images)
		case r.Method == http.MethodDelete && strings.HasPrefix(path, "/images/"):
			ref := strings.TrimPrefix(path, "/images/")
			f.removedImages = append(f.removedImages, ref)
			f.images = slices.DeleteFunc(f.images, func(img docker.ImageSummary) bool { return img.ID == ref || slices.Contains(img.RepoTags, ref) })
			io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && (path == "/build" || path == "/images/create" || strings.HasSuffix(path, "/tag")):
			f.builds = append(f.builds, r.Method+" "+path+"?"+r.URL.RawQuery)
			if hold := f.holdBuild; hold != nil && path == "/build" {
				f.mu.Unlock()
				<-hold
				f.mu.Lock()
			}
		case strings.HasPrefix(path, "/images/"):
			io.WriteString(w, `{"Config":{"Labels":{"boxrunner.image":"`+cmp.Or(f.built, runnerimage.Identity("2.338.0"))+`"}}}`)
		case path == "/containers/json":
			var list []map[string]any
			for _, c := range f.containers {
				if !c.matches(r.URL.Query().Get("filters")) {
					continue
				}
				list = append(list, map[string]any{"Id": c.Name, "Names": []string{"/" + c.Name}, "Labels": c.Labels, "State": c.State, "Created": c.Created, "ImageID": c.ImageID})
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
			f.containers[parts[1]].State = map[bool]string{false: "running", true: "exited"}[f.exitOnStart]
			w.WriteHeader(204)
		case len(parts) == 3 && parts[2] == "stop":
			if c, ok := f.containers[parts[1]]; ok {
				c.State = "exited"
			}
			f.stopped = append(f.stopped, parts[1])
			w.WriteHeader(204)
		case len(parts) == 3 && parts[2] == "logs":
			f.logs = append(f.logs, parts[1])
			io.WriteString(w, "\x02\x00\x00\x00\x00\x00\x00\x12exec format error\n")
		case r.Method == http.MethodDelete && parts[0] == "containers":
			if _, ok := f.containers[parts[1]]; !ok {
				http.Error(w, `{"message":"no such container"}`, 404)
				return
			}
			if f.failRemove[parts[1]] {
				http.Error(w, `{"message":"removal of container is already in progress"}`, http.StatusConflict)
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
	// paused are the classes paused on the dashboard.
	paused []string
	// heldBy, when set, is the other box whose session holds the scale set.
	heldBy string
	// fleet, when set, is the JSON the heartbeat's answer carries as the box's
	// fleet shares.
	fleet string
	// runner is the runner's latest release, 2.338.0 when empty. list is
	// runner-digests.json, which names digest338 for 2.338.0 when empty, and
	// tags maps each tag to the digest the registry names, 2.338.0's to
	// digest338 when nil.
	runner string
	list   string
	tags   map[string]string
	// org, when set, is the organization the Worker serves; it then lists
	// orgRepos, the private repositories its runner group admits, octo/site
	// and octo/docs when nil.
	org      string
	orgRepos []string
	// waiting is the jobs GET /v1/jobs lists, none when empty, and refuseMint
	// makes the Worker refuse every runner with repo_ineligible.
	waiting    string
	refuseMint bool
	// acquired are the bodies of the acquire calls, the jobs the agent took.
	acquired []string
}

const (
	digest338 = "sha256:4ffadc0002b2581327e06101fc8c06cd189232baf79fe561fac9caeb76f5e807"
	digest339 = "sha256:e5496277be5d09bc968b3d64911b74e219ac4a3f2edce956a3ecf9271bea1ef4"
)

func (g *fakeGitHub) pause(classes ...string) {
	g.mu.Lock()
	g.paused = classes
	g.mu.Unlock()
}

func (g *fakeGitHub) serve(t *testing.T) *httptest.Server {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// The Worker names itself in every answer, the heartbeat's included.
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			w.Header().Set("Boxrunner-Protocol", "1")
			w.Header().Set("Boxrunner-Worker-Version", "0.1.0")
		}
		// Heartbeats run on their own clock, so the call lists below leave them out.
		if strings.HasPrefix(r.URL.Path, "/v1/") && r.URL.Path != "/v1/heartbeat" {
			g.mu.Lock()
			g.calls = append(g.calls, strings.TrimSpace(fmt.Sprintf("%s %s %s", r.Method, r.URL.RequestURI(), body)))
			g.mu.Unlock()
		}
		session := fmt.Sprintf(`{"sessionId":"s1","queueUrl":"%s/queue/7","queueToken":"q1","acquireJobsUrl":"%s/acquire","statistics":{"totalAssignedJobs":0}}`, srv.URL, srv.URL)
		switch {
		case r.URL.Path == "/v1/repos":
			g.mu.Lock()
			paused := append([]string{}, g.paused...)
			g.mu.Unlock()
			serving := slices.DeleteFunc([]string{"boxrunner", "boxrunner-light"}, func(c string) bool { return slices.Contains(paused, c) })
			if g.org != "" {
				g.mu.Lock()
				names := g.orgRepos
				if names == nil {
					names = []string{"octo/site", "octo/docs"}
				}
				repos := []map[string]any{}
				for i, name := range names {
					repos = append(repos, map[string]any{"name": name, "id": 123 + i})
				}
				g.mu.Unlock()
				json.NewEncoder(w).Encode(map[string]any{
					"repos": repos, "classes": serving, "paused": paused,
					"organization": map[string]string{"name": g.org, "group": "boxrunner", "problem": ""},
				})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"repos": []map[string]any{{"name": "octo/site", "id": 123}}, "classes": serving, "paused": paused})
		case r.URL.Path == "/v1/scalesets":
			io.WriteString(w, `{"id":7,"name":"boxrunner"}`)
		case r.URL.Path == "/v1/sessions" && g.heldBy != "":
			w.WriteHeader(409)
			fmt.Fprintf(w, `{"error":"session_exists","message":"%s holds it","heldBy":%q}`, g.heldBy, g.heldBy)
		case r.URL.Path == "/v1/sessions":
			io.WriteString(w, session)
		case strings.HasPrefix(r.URL.Path, "/v1/sessions/"):
			w.WriteHeader(204)
		case r.URL.Path == "/v1/runners" && g.refuseMint:
			w.WriteHeader(410)
			io.WriteString(w, `{"error":"repo_ineligible","message":"octo/docs is not a repository this Worker serves, so it mints no runner for its job."}`)
		case r.URL.Path == "/v1/runners":
			io.WriteString(w, `{"runnerId":41,"jitConfig":"jit-41"}`)
		case r.URL.Path == "/v1/jobs":
			g.mu.Lock()
			fmt.Fprintf(w, `{"jobs":%s}`, cmp.Or(g.waiting, "[]"))
			g.mu.Unlock()
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
			g.mu.Lock()
			g.acquired = append(g.acquired, string(body))
			g.mu.Unlock()
			fmt.Fprintf(w, `{"count":1,"value":%s}`, body)
		case r.URL.Path == "/releases/latest":
			http.Redirect(w, r, "/releases/tag/v"+cmp.Or(g.runner, "2.338.0"), http.StatusFound)
		case r.URL.Path == "/runner-digests.json":
			io.WriteString(w, cmp.Or(g.list, `{"2.338.0":"`+digest338+`"}`))
		case r.URL.Path == "/token":
			io.WriteString(w, `{"token":"anonymous"}`)
		case r.Method == http.MethodHead && strings.HasPrefix(r.URL.Path, "/v2/actions/actions-runner/manifests/"):
			tags := g.tags
			if tags == nil {
				tags = map[string]string{"2.338.0": digest338}
			}
			w.Header().Set("Docker-Content-Digest", tags[strings.TrimPrefix(r.URL.Path, "/v2/actions/actions-runner/manifests/")])
		case r.URL.Path == "/dominikzabcik/boxrunner/releases/latest":
			http.Redirect(w, r, "/dominikzabcik/boxrunner/releases/tag/v0.2.2", http.StatusFound)
		case r.URL.Path == "/v1/heartbeat":
			g.mu.Lock()
			reply := map[string]any{"paused": append([]string{}, g.paused...)}
			if g.fleet != "" {
				reply["fleet"] = json.RawMessage(g.fleet)
			}
			json.NewEncoder(w).Encode(reply)
			g.mu.Unlock()
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
	drain  string
	socket string
	cancel context.CancelFunc
	done   chan error
	// cgroup holds the idle slice's cpu.idle, 0 at first.
	cgroup string
	// sysbox stands in for /var/lib/sysbox.
	sysbox string
	// run answers the commands the agent runs, except systemctl; a fakeHost
	// of its own when nil.
	run func(stdin, name string, args ...string) (string, error)
	// egressListen, openPackets and upstream stand in for the egress classes' sockets, log and resolver.
	egressListen func(egress.Net) (net.PacketConn, net.Listener, error)
	openPackets  func(uint16) (PacketSource, error)
	upstream     dnsfwd.Upstream
	// guardEvery is how often the egress rules are checked, 50 ms when zero.
	guardEvery time.Duration
	// reposEvery is how often an organization's repositories are listed, a minute when zero.
	reposEvery time.Duration
	// logs is what the agent logged.
	logs *logBuffer
	// metrics is where the agent serves metrics, none when zero.
	metrics netip.AddrPort
	// cache is the Actions cache's size, none when zero, served at cacheAddr.
	cache     config.Bytes
	cacheAddr netip.AddrPort

	mu        sync.Mutex
	systemctl []string
}

// startAgent runs the agent with one class; change may adjust the class and
// the fakes before the agent starts.
func startAgent(t *testing.T, containers map[string]*container, busy map[string]bool, change ...func(*config.Class, *harness)) *harness {
	h := &harness{
		docker: &fakeDocker{containers: containers, volumes: map[string]bool{}},
		github: &fakeGitHub{queue: make(chan string, 8), busy: busy},
		jobs:   filepath.Join(t.TempDir(), "jobs.jsonl"),
		drain:  filepath.Join(t.TempDir(), "draining"),
		socket: filepath.Join(t.TempDir(), "s.sock"),
		done:   make(chan error, 1),
		cgroup: t.TempDir(),
		sysbox: t.TempDir(),
		logs:   &logBuffer{},
	}
	os.Mkdir(filepath.Join(h.cgroup, "boxrunner_idle.slice"), 0o755)
	os.WriteFile(filepath.Join(h.cgroup, "boxrunner_idle.slice", "cpu.idle"), []byte("0\n"), 0o644)
	cfg := config.Config{
		Box: "box-1", DiskFloor: 1,
		Classes: []config.Class{{Name: "boxrunner", NanoCPUs: 2e9, Memory: 4 << 30, Pids: 4096, Shm: 1 << 30, Priority: config.Normal, Max: 1, Image: "boxrunner/runner:local"}},
	}
	for _, f := range change {
		f(&cfg.Classes[0], h)
	}
	if h.run == nil {
		h.run = (&fakeHost{}).run
	}
	cfg.Metrics, cfg.Cache = h.metrics, h.cache
	socket := h.docker.serve(t)
	srv := h.github.serve(t)
	d, err := docker.Dial(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(srv.URL)
	cfg.Worker = u
	log := slog.New(slog.NewTextHandler(h.logs, nil))
	var ctx context.Context
	ctx, h.cancel = context.WithCancel(context.Background())
	t.Cleanup(h.cancel)
	go func() {
		h.done <- Run(ctx, Options{
			Config: cfg, Docker: d, Version: "0.1.0", Log: log,
			Worker: worker.New(worker.Options{URL: u, Token: "box-token", Version: "0.1.0", Log: log}),
			Run: func(_ context.Context, stdin, name string, args ...string) (string, error) {
				if name == "systemctl" {
					h.mu.Lock()
					h.systemctl = append(h.systemctl, strings.Join(args, " "))
					h.mu.Unlock()
					return "", nil
				}
				return h.run(stdin, name, args...)
			},
			EgressListen:         h.egressListen,
			OpenPackets:          h.openPackets,
			EgressUpstream:       h.upstream,
			EgressCountEvery:     50 * time.Millisecond,
			EgressGuardEvery:     cmp.Or(h.guardEvery, 50*time.Millisecond),
			EgressRetry:          50 * time.Millisecond,
			JobsLog:              h.jobs,
			DrainFile:            h.drain,
			Socket:               h.socket,
			Runner:               runnerdigests.Source{Releases: srv.URL + "/releases/latest", List: srv.URL + "/runner-digests.json", Registry: srv.URL},
			BoxrunnerReleasesURL: srv.URL + "/dominikzabcik/boxrunner/releases/latest",
			Tick:                 50 * time.Millisecond,
			HeartbeatEvery:       50 * time.Millisecond,
			ReposEvery:           h.reposEvery,
			SysboxDir:            h.sysbox,
			CgroupDir:            h.cgroup,
			CacheDir:             t.TempDir(),
			CacheAddr:            h.cacheAddr,
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

// logBuffer keeps what the agent logs, and passes it on to stderr.
type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	l.b.Write(p)
	l.mu.Unlock()
	return os.Stderr.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// waitLog waits for the agent to log a line that has every part.
func (h *harness) waitLog(t *testing.T, parts ...string) string {
	t.Helper()
	var found string
	waitFor(t, "a log line with "+strings.Join(parts, " and "), func() bool {
		for _, line := range strings.Split(h.logs.String(), "\n") {
			if !slices.ContainsFunc(parts, func(p string) bool { return !strings.Contains(line, p) }) {
				found = line
				return true
			}
		}
		return false
	})
	return found
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

// The job log carries what the job's container used, read from its cgroup
// while it ran: Docker removes the cgroup with the container.
func TestJobUsage(t *testing.T) {
	h := startAgent(t, map[string]*container{}, nil)
	h.github.queue <- message(1, 1, `{"messageType":"JobAvailable","runnerRequestId":501}`)
	var name string
	waitFor(t, "a running job container", func() bool {
		for n, c := range h.docker.snapshot() {
			name = n
			return c.State == "running"
		}
		return false
	})
	scope := filepath.Join(h.cgroup, "system.slice", "docker-"+name+".scope")
	os.MkdirAll(scope, 0o755)
	os.WriteFile(filepath.Join(scope, "cpu.stat"), []byte("usage_usec 754250000\nuser_usec 700000000\nsystem_usec 54250000\n"), 0o644)
	os.WriteFile(filepath.Join(scope, "memory.peak"), []byte("9812000768\n"), 0o644)
	sampled := time.Now()
	waitFor(t, "a few samples", func() bool { return time.Since(sampled) > 300*time.Millisecond })
	os.RemoveAll(scope)

	h.github.queue <- message(2, 1, fmt.Sprintf(`{"messageType":"JobStarted","runnerRequestId":501,"runnerId":41,"runnerName":%q}`, name))
	h.github.queue <- message(3, 0, fmt.Sprintf(`{"messageType":"JobCompleted","runnerRequestId":501,"runnerId":41,"runnerName":%q,"result":"succeeded","jobDisplayName":"test",
		"finishTime":"2026-10-13T21:03:00Z"}`, name))
	waitFor(t, "the finished job's container to go", func() bool { return len(h.docker.snapshot()) == 0 })
	h.stop(t)

	jobs, _ := status.Recent(h.jobs, 10)
	if len(jobs) != 1 || jobs[0].Usage == nil || *jobs[0].Usage != (status.Usage{CPUSeconds: 754.25, MemoryPeak: 9812000768}) {
		t.Errorf("job log = %+v", jobs)
	}
}

// An organization scale set is offered the jobs of every repository its
// runner group admits; the agent takes only those of repositories both the
// Worker and the class serve, names their repositories when it mints a
// runner, and records each job under its own repository. GitHub offers a job
// once, so a job turned down while the Worker did not serve its repository is
// found among the waiting jobs once it does.
func TestOrganizationScaleSet(t *testing.T) {
	h := startAgent(t, map[string]*container{}, nil, func(c *config.Class, h *harness) {
		h.github.org = "octo"
		h.github.orgRepos = []string{"octo/site"}
		h.reposEvery = 50 * time.Millisecond
		c.Repos = []string{"octo/site", "octo/docs", "octo/secret"}
	})
	h.github.queue <- message(1, 1,
		`{"messageType":"JobAvailable","runnerRequestId":501,"repositoryName":"site","ownerName":"octo","queueTime":"2026-10-13T21:00:00Z"}`,
		`{"messageType":"JobAvailable","runnerRequestId":502,"repositoryName":"docs","ownerName":"octo","jobDisplayName":"lint"}`,
		`{"messageType":"JobAvailable","runnerRequestId":503,"repositoryName":"secret","ownerName":"octo"}`)

	var name string
	waitFor(t, "a running job container", func() bool {
		for n, c := range h.docker.snapshot() {
			name = n
			return c.State == "running"
		}
		return false
	})
	h.waitLog(t, "was offered a job of a repository this box does not serve", "repo=octo/docs", "job=lint")
	h.github.queue <- message(2, 1, fmt.Sprintf(`{"messageType":"JobStarted","runnerRequestId":501,"runnerId":41,"runnerName":%q,"repositoryName":"site","ownerName":"octo"}`, name))
	h.github.queue <- message(3, 0, fmt.Sprintf(`{"messageType":"JobCompleted","runnerRequestId":501,"runnerId":41,"runnerName":%q,"result":"succeeded","jobDisplayName":"test",
		"repositoryName":"site","ownerName":"octo","queueTime":"2026-10-13T21:00:00Z","runnerAssignTime":"2026-10-13T21:00:08Z","finishTime":"2026-10-13T21:03:00Z"}`, name))
	waitFor(t, "the finished job's container to go", func() bool { return len(h.docker.snapshot()) == 0 })

	// The Worker now serves octo/docs; its job still waits, and GitHub does not offer it again.
	h.github.mu.Lock()
	h.github.orgRepos = []string{"octo/site", "octo/docs"}
	h.github.waiting = `[{"runnerRequestId":502,"repositoryName":"docs","ownerName":"octo"}]`
	h.github.mu.Unlock()
	waitFor(t, "octo/docs's waiting job to be acquired", func() bool {
		h.github.mu.Lock()
		defer h.github.mu.Unlock()
		return len(h.github.acquired) == 2
	})
	snap, err := status.Fetch(context.Background(), h.socket)
	if err != nil {
		t.Fatal(err)
	}
	h.stop(t)

	var calls []string
	for _, call := range h.github.workerCalls() {
		if call != "GET /v1/repos" {
			calls = append(calls, call)
		}
	}
	wantCalls := []string{
		`POST /v1/scalesets {"class":"boxrunner","org":"octo"}`,
		`POST /v1/sessions {"org":"octo","owner":"box-1","scaleSetId":7}`,
		"GET /v1/jobs?org=octo&scaleSetId=7",
		fmt.Sprintf(`POST /v1/runners {"jobRepos":["octo/site"],"name":%q,"org":"octo","scaleSetId":7}`, name),
		"GET /v1/jobs?org=octo&scaleSetId=7",
		"DELETE /v1/sessions/s1?org=octo&scaleSetId=7",
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Errorf("worker calls =\n%q\nwant\n%q", calls, wantCalls)
	}
	if got := h.github.acquired; !reflect.DeepEqual(got, []string{"[501]", "[502]"}) {
		t.Errorf("acquired %q, want octo/site's job, then octo/docs's", got)
	}
	wantSets := []status.ScaleSet{{Repo: "octo", Class: "boxrunner", Refused: map[string]int{"octo/docs": 1, "octo/secret": 1}}}
	if !reflect.DeepEqual(snap.ScaleSets, wantSets) {
		t.Errorf("scale sets = %+v, want %+v", snap.ScaleSets, wantSets)
	}
	if want := (&status.Organization{Name: "octo", Group: "boxrunner"}); !reflect.DeepEqual(snap.Organization, want) {
		t.Errorf("organization = %+v, want %+v", snap.Organization, want)
	}
	jobs, _ := status.Recent(h.jobs, 10)
	if len(jobs) != 1 || jobs[0].Repo != "octo/site" {
		t.Errorf("job log = %+v; want the job under octo/site", jobs)
	}
}

// While an organization scale set holds a job of a repository the Worker no
// longer serves, the Worker refuses every runner; the class backs off rather
// than asking every tick.
func TestOrganizationMintRefused(t *testing.T) {
	h := startAgent(t, map[string]*container{}, nil, func(_ *config.Class, h *harness) {
		h.github.org = "octo"
		h.github.refuseMint = true
	})
	h.github.queue <- message(1, 1, `{"messageType":"JobAvailable","runnerRequestId":501,"repositoryName":"docs","ownerName":"octo"}`)
	var held time.Time
	waitFor(t, "the class to hold its starts", func() bool {
		snap, err := status.Fetch(context.Background(), h.socket)
		if err != nil || len(snap.Classes) == 0 {
			return false
		}
		held = snap.Classes[0].HeldUntil
		return !held.IsZero()
	})
	h.waitLog(t, "the Worker refuses a runner while the organization's scale set holds a job of a repository it does not serve", "octo/docs is not a repository this Worker serves")
	h.stop(t)
	mints := 0
	for _, call := range h.github.workerCalls() {
		if strings.HasPrefix(call, "POST /v1/runners ") {
			mints++
			if !strings.Contains(call, `"jobRepos":["octo/docs"]`) {
				t.Errorf("mint %s does not name octo/docs", call)
			}
		}
	}
	if mints != 3 {
		t.Errorf("%d mints before the class held its starts, want 3", mints)
	}
	if time.Until(held) < 50*time.Second {
		t.Errorf("held until %v, want about a minute from now", held)
	}
}

// The repository list is read every five minutes, so only the heartbeat's
// answer can make the agent act on a pause within this test's ten seconds.
func TestPauseAndResume(t *testing.T) {
	h := startAgent(t, map[string]*container{}, nil)
	defer h.stop(t)
	count := func(call string) func() int {
		return func() int {
			n := 0
			for _, c := range h.github.workerCalls() {
				if strings.HasPrefix(c, call) {
					n++
				}
			}
			return n
		}
	}
	opened, closed := count("POST /v1/sessions"), count("DELETE /v1/sessions/s1")
	waitFor(t, "a session", func() bool { return opened() == 1 })
	h.github.pause("boxrunner")
	waitFor(t, "the paused class's session to close", func() bool { return closed() == 1 })
	h.github.pause()
	waitFor(t, "a session after resuming", func() bool { return opened() == 2 })
	if closed() != 1 {
		t.Errorf("sessions closed while resumed: %d", closed())
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

func TestStopRemovesSocket(t *testing.T) {
	h := startAgent(t, map[string]*container{}, nil)
	waitFor(t, "the status socket", func() bool {
		_, err := status.Fetch(context.Background(), h.socket)
		return err == nil
	})
	h.stop(t)
	if _, err := os.Stat(h.socket); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("after Run returned, the status socket is still there: %v", err)
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

// A warm repository gets a runner before any job, status marks it, and
// stopping the agent removes it like any runner without a job.
func TestWarmRunner(t *testing.T) {
	h := startAgent(t, map[string]*container{}, nil, func(c *config.Class, _ *harness) {
		c.Repos, c.Warm = []string{"octo/site"}, []string{"octo/site"}
	})
	var snap status.Snapshot
	waitFor(t, "a warm runner in status", func() bool {
		var err error
		snap, err = status.Fetch(context.Background(), h.socket)
		return err == nil && len(snap.Runners) == 1 && snap.Runners[0].Phase == "starting"
	})
	if r := snap.Runners[0]; !r.Warm || r.Repo != "octo/site" || !snap.FreshStart {
		t.Errorf("status = %+v", snap)
	}
	h.stop(t)
	if left := h.docker.snapshot(); len(left) != 0 {
		t.Errorf("containers left after stop: %v", left)
	}
	if calls := h.github.workerCalls(); !slices.Contains(calls, "DELETE /v1/runners/41?repo=octo%2Fsite&scaleSetId=7") {
		t.Errorf("worker calls = %q", calls)
	}
}

// Draining closes the session and removes the warm runner but keeps the
// running job; once that job's container exits the box is drained. The drain
// outlasts a restart, and resuming opens the session again.
func TestDrainAndResume(t *testing.T) {
	labels := map[string]string{"boxrunner.box": "box-1", "boxrunner.runner": "box-1-boxrunner-aaaaaa", "boxrunner.runner-id": "40",
		"boxrunner.repo": "octo/site", "boxrunner.class": "boxrunner", "boxrunner.scaleset-id": "7"}
	h := startAgent(t, map[string]*container{
		"box-1-boxrunner-aaaaaa": {Name: "box-1-boxrunner-aaaaaa", Labels: labels, State: "running", Created: time.Now().Unix()},
	}, map[string]bool{"40": true}, func(c *config.Class, _ *harness) {
		c.Max, c.Repos, c.Warm = 2, []string{"octo/site"}, []string{"octo/site"}
	})
	fetch := func() status.Snapshot {
		snap, _ := status.Fetch(context.Background(), h.socket)
		return snap
	}
	h.github.queue <- message(1, 1, `{"messageType":"JobStarted","runnerRequestId":501,"runnerId":40,"runnerName":"box-1-boxrunner-aaaaaa"}`)
	waitFor(t, "a warm runner beside the job", func() bool { return len(h.docker.snapshot()) == 2 })
	if err := status.Drain(context.Background(), h.socket, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the warm runner to go", func() bool { return len(h.docker.snapshot()) == 1 })
	var snap status.Snapshot
	waitFor(t, "a closed session", func() bool { snap = fetch(); return snap.Draining && len(snap.ScaleSets) == 0 })
	if snap.Drained() || len(snap.Runners) != 1 || snap.Runners[0].Name != "box-1-boxrunner-aaaaaa" || h.docker.snapshot()["box-1-boxrunner-aaaaaa"].State != "running" {
		t.Errorf("status while the job runs = %+v", snap)
	}
	if _, err := os.Stat(h.drain); err != nil {
		t.Errorf("drain file: %v", err)
	}
	h.docker.mu.Lock()
	h.docker.containers["box-1-boxrunner-aaaaaa"].State = "exited"
	h.docker.mu.Unlock()
	waitFor(t, "a drained box", func() bool { return fetch().Drained() })
	time.Sleep(300 * time.Millisecond)
	h.stop(t)
	want := []string{
		"GET /v1/repos",
		`POST /v1/scalesets {"class":"boxrunner","repo":"octo/site"}`,
		`POST /v1/sessions {"owner":"box-1","repo":"octo/site","scaleSetId":7}`,
		`POST /v1/runners {"name":"WARM","repo":"octo/site","scaleSetId":7}`,
		"DELETE /v1/runners/41?repo=octo%2Fsite&scaleSetId=7",
		"DELETE /v1/sessions/s1?repo=octo%2Fsite&scaleSetId=7",
	}
	calls := h.github.workerCalls()
	for i, c := range calls {
		if strings.HasPrefix(c, "POST /v1/runners ") {
			calls[i] = `POST /v1/runners {"name":"WARM","repo":"octo/site","scaleSetId":7}`
		}
	}
	slices.Sort(calls)
	slices.Sort(want)
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("worker calls =\n%q\nwant\n%q", calls, want)
	}

	drain := h.drain
	h = startAgent(t, map[string]*container{}, nil, func(_ *config.Class, h *harness) { h.drain = drain })
	waitFor(t, "status after the restart", func() bool { return fetch().Draining })
	time.Sleep(300 * time.Millisecond)
	if calls := h.github.workerCalls(); len(calls) != 0 {
		t.Errorf("worker calls of a draining agent = %q", calls)
	}
	if err := status.Drain(context.Background(), h.socket, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a session after resuming", func() bool {
		return slices.Contains(h.github.workerCalls(), `POST /v1/sessions {"owner":"box-1","repo":"octo/site","scaleSetId":7}`)
	})
	if _, err := os.Stat(drain); !os.IsNotExist(err) {
		t.Errorf("drain file after resuming: %v", err)
	}
	if fetch().Draining {
		t.Error("status still says draining")
	}
	h.stop(t)
}

// A scale set another box's session holds shows in status as that box's, the
// agent does not keep asking for it, and status does not point to replacing a
// box.
func TestScaleSetHeldByAnotherBox(t *testing.T) {
	h := startAgent(t, map[string]*container{}, nil, func(_ *config.Class, h *harness) {
		h.github.heldBy = "box-2"
		if err := os.WriteFile(h.jobs, []byte(`{"repo":"octo/site","class":"boxrunner"}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	var snap status.Snapshot
	waitFor(t, "the other box in status", func() bool {
		var err error
		snap, err = status.Fetch(context.Background(), h.socket)
		return err == nil && len(snap.Elsewhere) > 0
	})
	time.Sleep(300 * time.Millisecond)
	h.stop(t)
	want := []status.Elsewhere{{Repo: "octo/site", Class: "boxrunner", Box: "box-2"}}
	if !reflect.DeepEqual(snap.Elsewhere, want) || snap.FreshStart || len(snap.ScaleSets) != 0 {
		t.Errorf("status = %+v", snap)
	}
	opened := 0
	for _, c := range h.github.workerCalls() {
		if strings.HasPrefix(c, "POST /v1/sessions ") {
			opened++
		}
	}
	if opened != 1 {
		t.Errorf("%d session requests; want 1 until the next repository check", opened)
	}
}

// A box whose session another box holds starts runners for the share the
// Worker gives it, and none once the share is gone.
func TestFleetShare(t *testing.T) {
	h := startAgent(t, map[string]*container{}, nil, func(_ *config.Class, h *harness) {
		h.github.heldBy = "box-2"
		h.github.fleet = `[{"repo":"octo/site","class":"boxrunner","jobs":1,"repos":[]}]`
	})
	defer h.stop(t)
	minted := func() int {
		n := 0
		for _, c := range h.github.workerCalls() {
			if strings.HasPrefix(c, "POST /v1/runners ") {
				n++
			}
		}
		return n
	}
	waitFor(t, "a runner minted for the fleet share", func() bool { return minted() == 1 })
	waitFor(t, "its container running", func() bool {
		for _, c := range h.docker.snapshot() {
			return c.State == "running"
		}
		return false
	})
	time.Sleep(300 * time.Millisecond)
	if n := minted(); n != 1 {
		t.Errorf("%d runners minted for a share of 1", n)
	}
}

// systemd before 252 ignores CPUWeight=idle, so the agent sets the idle
// slice's cpu.idle when it starts, and again before an idle job when a
// restart of the slice has reset it.
func TestIdleSlice(t *testing.T) {
	h := startAgent(t, map[string]*container{}, nil, func(c *config.Class, _ *harness) { c.Priority = config.Idle })
	file := filepath.Join(h.cgroup, "boxrunner_idle.slice", "cpu.idle")
	idle := func() string {
		b, _ := os.ReadFile(file)
		return strings.TrimSpace(string(b))
	}
	waitFor(t, "cpu.idle 1 once the agent started", func() bool { return idle() == "1" })
	os.WriteFile(file, []byte("0\n"), 0o644)

	h.github.queue <- message(1, 1, `{"messageType":"JobAvailable","runnerRequestId":501,"queueTime":"2026-10-13T21:00:00Z"}`)
	waitFor(t, "a job container", func() bool { return len(h.docker.snapshot()) == 1 })
	if got := idle(); got != "1" {
		t.Errorf("cpu.idle when the idle job's container was created = %q, want 1", got)
	}
	h.stop(t)
	h.mu.Lock()
	defer h.mu.Unlock()
	if want := []string{"start boxrunner_idle.slice", "start boxrunner_idle.slice"}; !reflect.DeepEqual(h.systemctl, want) {
		t.Errorf("systemctl calls = %q, want %q", h.systemctl, want)
	}
}

func TestSpecIdleClass(t *testing.T) {
	class := config.Class{Name: "boxrunner-light", NanoCPUs: 4e9, Memory: 2 << 30, Pids: 512, Shm: 1 << 30, Priority: config.Idle, Image: "boxrunner/runner:local"}
	r := scheduler.Runner{Name: "box-1-boxrunner-light-3f9a2c", ScaleSet: scheduler.Key{Repo: "octo/site", Class: "boxrunner-light"}, ScaleSetID: 8, ID: 41}
	got := Spec("box-1", class, r, "jit", "http://172.18.0.1:5000", "http://172.18.0.1:5001", nil)
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
	class := config.Class{Name: "boxrunner", NanoCPUs: 2e9, Memory: 4 << 30, Pids: 4096, Shm: 1 << 30, Priority: config.Normal, Image: "boxrunner/runner:local", Docker: true, Isolation: config.Sysbox}
	r := scheduler.Runner{Name: "box-1-boxrunner-3f9a2c", ScaleSet: scheduler.Key{Repo: "octo/site", Class: "boxrunner"}, ScaleSetID: 7, ID: 41}
	got := Spec("box-1", class, r, "jit", "http://172.18.0.1:5000", "http://172.18.0.1:5001", nil)
	want := docker.ContainerSpec{
		Name: "box-1-boxrunner-3f9a2c", Image: "boxrunner/runner:local",
		Env: []string{"ACTIONS_RUNNER_INPUT_JITCONFIG=jit", "BOXRUNNER_DOCKER=1", "BOXRUNNER_MIRROR=http://172.18.0.1:5000"},
		Labels: map[string]string{"boxrunner.box": "box-1", "boxrunner.runner": "box-1-boxrunner-3f9a2c", "boxrunner.runner-id": "41",
			"boxrunner.repo": "octo/site", "boxrunner.class": "boxrunner", "boxrunner.scaleset-id": "7"},
		NanoCPUs: 2e9, Memory: 4 << 30, PidsLimit: 4096, ShmSize: 1 << 30,
		Network: "boxrunner", Runtime: "sysbox-runc",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Spec =\n%+v\nwant\n%+v", got, want)
	}
}

func TestSpecCacheClass(t *testing.T) {
	class := config.Class{Name: "boxrunner", NanoCPUs: 2e9, Memory: 4 << 30, Pids: 4096, Shm: 1 << 30, Priority: config.Normal, Image: "boxrunner/runner:local", Cache: true}
	r := scheduler.Runner{Name: "box-1-boxrunner-3f9a2c", ScaleSet: scheduler.Key{Repo: "octo/site", Class: "boxrunner"}, ScaleSetID: 7, ID: 41}
	for _, c := range []struct {
		cache bool
		url   string
		want  []string
	}{
		{true, "http://172.18.0.1:5001", []string{"ACTIONS_RUNNER_INPUT_JITCONFIG=jit", "BOXRUNNER_CACHE=http://172.18.0.1:5001"}},
		{true, "", []string{"ACTIONS_RUNNER_INPUT_JITCONFIG=jit"}},
		{false, "http://172.18.0.1:5001", []string{"ACTIONS_RUNNER_INPUT_JITCONFIG=jit"}},
	} {
		class.Cache = c.cache
		if got := Spec("box-1", class, r, "jit", "", c.url, nil); !reflect.DeepEqual(got.Env, c.want) {
			t.Errorf("Spec with cache = %v and the cache at %q: env %q; want %q", c.cache, c.url, got.Env, c.want)
		}
	}
}

// TestCache starts the agent with a cache: it serves at its address, and a
// job of a class with cache = true gets that address.
func TestCache(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := netip.MustParseAddrPort(l.Addr().String())
	l.Close()
	h := startAgent(t, map[string]*container{}, nil, func(c *config.Class, h *harness) {
		c.Cache, h.cache, h.cacheAddr = true, config.GiB, addr
	})
	h.github.queue <- message(1, 1, `{"messageType":"JobAvailable","runnerRequestId":501,"queueTime":"2026-10-13T21:00:00Z"}`)
	var env []any
	waitFor(t, "a job container", func() bool {
		for _, c := range h.docker.snapshot() {
			env, _ = c.Body["Env"].([]any)
			return true
		}
		return false
	})
	if want := "BOXRUNNER_CACHE=http://" + addr.String(); !slices.Contains(env, any(want)) {
		t.Errorf("the job's env = %q; want it to hold %s", env, want)
	}
	resp, err := http.Get("http://" + addr.String() + "/_boxrunner/upstream")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "https://results-receiver.actions.githubusercontent.com" {
		t.Errorf("the cache names upstream %q", body)
	}
	h.stop(t)
	if _, err := http.Get("http://" + addr.String() + "/_boxrunner/upstream"); err == nil {
		t.Errorf("the cache still answers after the agent stopped")
	}
}

func TestSpecSysboxClass(t *testing.T) {
	class := config.Class{Name: "boxrunner", NanoCPUs: 2e9, Memory: 4 << 30, Pids: 4096, Shm: 1 << 30, Priority: config.Normal, Image: "boxrunner/runner:local", Isolation: config.Sysbox}
	r := scheduler.Runner{Name: "box-1-boxrunner-3f9a2c", ScaleSet: scheduler.Key{Repo: "octo/site", Class: "boxrunner"}, ScaleSetID: 7, ID: 41}
	got := Spec("box-1", class, r, "jit", "http://172.18.0.1:5000", "http://172.18.0.1:5001", nil)
	if got.Runtime != "sysbox-runc" || !reflect.DeepEqual(got.Env, []string{"ACTIONS_RUNNER_INPUT_JITCONFIG=jit"}) {
		t.Errorf("Spec runtime %q, env %q; want sysbox-runc and no Docker of the job's own", got.Runtime, got.Env)
	}
}

// TestRuntimeFollowsIsolation starts a job of a class on boxes with and
// without Sysbox and reads the runtime its container was created with.
func TestRuntimeFollowsIsolation(t *testing.T) {
	cases := []struct {
		isolation config.Isolation
		sysbox    bool
		want      string
	}{
		{config.AutoIsolation, false, ""},
		{config.AutoIsolation, true, "sysbox-runc"},
		{config.Runc, true, ""},
		{config.Sysbox, true, "sysbox-runc"},
	}
	// Not subtests: their longer temporary paths overflow a Unix socket's name.
	for _, c := range cases {
		h := startAgent(t, map[string]*container{}, nil, func(k *config.Class, h *harness) {
			k.Isolation = c.isolation
			if c.sysbox {
				h.docker.runtimes = []string{"sysbox-runc"}
			}
		})
		h.github.queue <- message(1, 1, `{"messageType":"JobAvailable","runnerRequestId":501}`)
		var host map[string]any
		waitFor(t, "a running job container", func() bool {
			for _, k := range h.docker.snapshot() {
				if k.Labels["boxrunner.box"] == "box-1" && k.State == "running" {
					host = k.Body["HostConfig"].(map[string]any)
					return true
				}
			}
			return false
		})
		h.stop(t)
		if got, _ := host["Runtime"].(string); got != c.want {
			t.Errorf("isolation %s, Sysbox installed %v: runtime = %q, want %q", c.isolation, c.sysbox, got, c.want)
		}
	}
}

func TestDockerClassNeedsSysbox(t *testing.T) {
	without := startAgent(t, map[string]*container{}, nil, func(c *config.Class, _ *harness) { c.Docker, c.Isolation = true, config.Sysbox })
	waitFor(t, "the repository list", func() bool { return len(without.github.workerCalls()) > 0 })
	time.Sleep(300 * time.Millisecond)
	without.stop(t)
	if got := without.github.workerCalls(); !reflect.DeepEqual(got, []string{"GET /v1/repos"}) {
		t.Errorf("without Sysbox, worker calls = %q; want only the repository list", got)
	}

	with := startAgent(t, map[string]*container{}, nil, func(c *config.Class, h *harness) {
		c.Docker, c.Isolation = true, config.Sysbox
		h.docker.runtimes = []string{"sysbox-runc"}
	})
	with.github.queue <- message(1, 1, `{"messageType":"JobAvailable","runnerRequestId":501}`)
	var body map[string]any
	waitFor(t, "a running job container", func() bool {
		for _, c := range with.docker.snapshot() {
			if c.Labels["boxrunner.box"] == "box-1" {
				body = c.Body
				return c.State == "running"
			}
		}
		return false
	})
	with.stop(t)
	host := body["HostConfig"].(map[string]any)
	if host["Runtime"] != "sysbox-runc" || !reflect.DeepEqual(body["Env"], []any{"ACTIONS_RUNNER_INPUT_JITCONFIG=jit-41", "BOXRUNNER_DOCKER=1", "BOXRUNNER_MIRROR=http://172.18.0.1:5000"}) {
		t.Errorf("with Sysbox, runtime %v and env %v", host["Runtime"], body["Env"])
	}
	m := with.docker.snapshot()["boxrunner-mirror"]
	with.docker.mu.Lock()
	volume := with.docker.volumes["boxrunner-mirror"]
	with.docker.mu.Unlock()
	mirrorHost, _ := json.Marshal(m.Body["HostConfig"])
	if m.State != "running" || !reflect.DeepEqual(m.Body["Env"], []any{"REGISTRY_PROXY_REMOTEURL=https://registry-1.docker.io", "REGISTRY_HTTP_ADDR=172.18.0.1:5000"}) ||
		string(mirrorHost) != `{"Mounts":[{"Source":"boxrunner-mirror","Target":"/var/lib/registry","Type":"volume"}],"NetworkMode":"host","RestartPolicy":{"Name":"unless-stopped"}}` ||
		!volume {
		t.Errorf("mirror %s with env %v and host config %s; volume %v", m.State, m.Body["Env"], mirrorHost, volume)
	}
}

func TestWithInnerDockerCountsSysboxData(t *testing.T) {
	sysbox := t.TempDir()
	job, other := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for path, size := range map[string]int{"docker/" + job + "/overlay2/layer": 3000, "buildkit/" + job + "/cache": 96, "docker/" + other + "/overlay2/layer": 7} {
		os.MkdirAll(filepath.Join(sysbox, filepath.Dir(path)), 0o755)
		os.WriteFile(filepath.Join(sysbox, path), make([]byte, size), 0o644)
	}
	list := []docker.Container{{ID: job, SizeRw: 1000}, {ID: strings.Repeat("c", 64), SizeRw: 5}, {SizeRw: 1}}
	withInnerDocker(list, sysbox)
	if got := []int64{list[0].SizeRw, list[1].SizeRw, list[2].SizeRw}; !reflect.DeepEqual(got, []int64{4096, 5, 1}) {
		t.Errorf("sizes = %v", got)
	}
}

func TestSysboxSweep(t *testing.T) {
	live, gone := strings.Repeat("a", 64), strings.Repeat("b", 64)
	h := startAgent(t, map[string]*container{live: {Name: live, State: "running"}}, nil, func(c *config.Class, h *harness) {
		c.Docker, c.Isolation = true, config.Sysbox
		h.docker.runtimes = []string{"sysbox-runc"}
		for _, dir := range []string{"docker/" + live, "docker/" + gone, "buildkit/" + gone, "docker/not-an-id"} {
			os.MkdirAll(filepath.Join(h.sysbox, dir, "overlay2"), 0o755)
		}
	})
	waitFor(t, "the removed container's directories gone", func() bool {
		left, _ := filepath.Glob(filepath.Join(h.sysbox, "*", "*"))
		return len(left) == 2
	})
	h.stop(t)
	left, _ := filepath.Glob(filepath.Join(h.sysbox, "*", "*"))
	if want := []string{filepath.Join(h.sysbox, "docker", live), filepath.Join(h.sysbox, "docker", "not-an-id")}; !reflect.DeepEqual(left, want) {
		t.Errorf("left %q; want %q", left, want)
	}
}

func TestMirrorOnlyForDockerClasses(t *testing.T) {
	h := startAgent(t, map[string]*container{}, nil)
	waitFor(t, "the repository list", func() bool { return len(h.github.workerCalls()) > 0 })
	h.stop(t)
	if got := h.docker.snapshot(); len(got) != 0 {
		t.Errorf("without a docker class, containers = %v", got)
	}
}

// Only a box without job history reads a start without containers as a new
// box that may have taken over from a dead one.
func TestHasEntries(t *testing.T) {
	dir := t.TempDir()
	empty, full := filepath.Join(dir, "empty.jsonl"), filepath.Join(dir, "full.jsonl")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(`{"repo":"octo/site"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{filepath.Join(dir, "missing.jsonl"): false, empty: false, full: true} {
		if got := hasEntries(path); got != want {
			t.Errorf("hasEntries(%s) = %v, want %v", filepath.Base(path), got, want)
		}
	}
}

// Containers that exit before taking a job: after three, the class holds its
// starts for a minute, logs the third one's output and status says so.
func TestCrashLoopHoldsTheClass(t *testing.T) {
	h := startAgent(t, map[string]*container{}, nil, func(_ *config.Class, h *harness) { h.docker.exitOnStart = true })
	h.github.queue <- message(1, 1, `{"messageType":"JobAvailable","runnerRequestId":501}`)
	var snap status.Snapshot
	waitFor(t, "the class to hold", func() bool {
		var err error
		snap, err = status.Fetch(context.Background(), h.socket)
		return err == nil && len(snap.Classes) == 1 && !snap.Classes[0].HeldUntil.IsZero()
	})
	if wait := time.Until(snap.Classes[0].HeldUntil); wait < 50*time.Second || wait > time.Minute {
		t.Errorf("held for %s, want a minute", wait)
	}
	time.Sleep(500 * time.Millisecond)
	h.stop(t)
	minted := 0
	for _, c := range h.github.workerCalls() {
		if strings.HasPrefix(c, "POST /v1/runners") {
			minted++
		}
	}
	h.docker.mu.Lock()
	logs := slices.Clone(h.docker.logs)
	h.docker.mu.Unlock()
	if minted != 3 || len(logs) != 1 || len(h.docker.snapshot()) != 0 {
		t.Errorf("minted %d runners, read the output of %v, left %v; want 3, one and none", minted, logs, h.docker.snapshot())
	}
}

// On start the agent removes the job image of runner 2.337.0 and the tags of
// old runner images, keeps the 2.336.0 job image another container uses, and
// status shows what the rest takes.
func TestStartRemovesSupersededImages(t *testing.T) {
	job := func(id, version string, size int64, tags ...string) docker.ImageSummary {
		return docker.ImageSummary{ID: id, RepoTags: tags, Labels: map[string]string{"boxrunner.image": version + "+3f9a2c0b1d4e"}, Size: size}
	}
	runner := func(id, version string, size int64) docker.ImageSummary {
		return docker.ImageSummary{ID: id, RepoTags: []string{"ghcr.io/actions/actions-runner:" + version}, Size: size}
	}
	h := startAgent(t, map[string]*container{"other": {Name: "other", State: "running", ImageID: "sha256:j336"}}, nil, func(_ *config.Class, h *harness) {
		h.docker.images = []docker.ImageSummary{
			runner("sha256:b338", "2.338.0", 1_000_000_000), job("sha256:j338", "2.338.0", 1_500_000_000, "boxrunner/runner:local"),
			runner("sha256:b337", "2.337.0", 990_000_000), job("sha256:j337", "2.337.0", 1_490_000_000),
			runner("sha256:b336", "2.336.0", 980_000_000), job("sha256:j336", "2.336.0", 1_480_000_000),
		}
	})
	var snap status.Snapshot
	waitFor(t, "the image disk use in status", func() bool {
		var err error
		snap, err = status.Fetch(context.Background(), h.socket)
		return err == nil && snap.Image.Disk > 0
	})
	h.stop(t)
	h.docker.mu.Lock()
	removed := slices.Clone(h.docker.removedImages)
	h.docker.mu.Unlock()
	if want := []string{"sha256:j337", "ghcr.io/actions/actions-runner:2.336.0", "ghcr.io/actions/actions-runner:2.337.0"}; !reflect.DeepEqual(removed, want) {
		t.Errorf("removed images %q, want %q", removed, want)
	}
	if snap.Image.Disk != 2_980_000_000 {
		t.Errorf("image disk use = %d, want 2980000000", snap.Image.Disk)
	}
}

// While boxrunner's CI has not verified the runner's latest release, the agent
// builds the newest release it did verify, from the listed digest, and status
// says why.
func TestBuildsTheNewestListedRunner(t *testing.T) {
	h := startAgent(t, map[string]*container{}, nil, func(_ *config.Class, h *harness) {
		h.docker.built = runnerimage.Identity("2.337.0")
		h.github.runner = "2.339.0"
	})
	var snap status.Snapshot
	waitFor(t, "the job image built in status", func() bool {
		var err error
		snap, err = status.Fetch(context.Background(), h.socket)
		return err == nil && snap.Image.Digest != ""
	})
	h.stop(t)
	snap.Image.Disk = 0
	want := status.ImageStatus{Built: runnerimage.Identity("2.338.0"), Digest: digest338, Latest: runnerimage.Identity("2.338.0"), Unlisted: "2.339.0"}
	if snap.Image != want {
		t.Errorf("image status = %+v, want %+v", snap.Image, want)
	}
	h.docker.mu.Lock()
	defer h.docker.mu.Unlock()
	build := "POST /build?buildargs=" + url.QueryEscape(`{"RUNNER_DIGEST":"`+digest338+`","RUNNER_VERSION":"2.338.0"}`) +
		"&forcerm=1&labels=" + url.QueryEscape(`{"boxrunner.image":"`+runnerimage.Identity("2.338.0")+`","boxrunner.runner-digest":"`+digest338+`"}`) +
		"&rm=1&t=" + url.QueryEscape("boxrunner/runner:local")
	if want := []string{
		"POST /images/create?fromImage=" + url.QueryEscape("ghcr.io/actions/actions-runner@"+digest338),
		"POST /images/ghcr.io/actions/actions-runner@" + digest338 + "/tag?repo=" + url.QueryEscape("ghcr.io/actions/actions-runner") + "&tag=2.338.0",
		build,
	}; !reflect.DeepEqual(h.docker.builds, want) {
		t.Errorf("Docker calls\n%q\nwant\n%q", h.docker.builds, want)
	}
}

// While the agent builds a newer job image, status shows it beside the one
// jobs keep using until the build is done.
func TestJobImageBuilding(t *testing.T) {
	hold := make(chan struct{})
	h := startAgent(t, map[string]*container{}, nil, func(_ *config.Class, h *harness) {
		h.docker.built = runnerimage.Identity("2.337.0")
		h.docker.holdBuild = hold
	})
	var snap status.Snapshot
	waitFor(t, "the build in status", func() bool {
		var err error
		snap, err = status.Fetch(context.Background(), h.socket)
		return err == nil && snap.Image.Building == runnerimage.Identity("2.338.0")
	})
	snap.Image.Disk = 0
	want := status.ImageStatus{Built: runnerimage.Identity("2.337.0"), Latest: runnerimage.Identity("2.338.0"), Building: runnerimage.Identity("2.338.0")}
	if snap.Image != want {
		t.Errorf("image status while building = %+v, want %+v", snap.Image, want)
	}
	close(hold)
	waitFor(t, "the built image in status", func() bool {
		var err error
		snap, err = status.Fetch(context.Background(), h.socket)
		return err == nil && snap.Image.Built == runnerimage.Identity("2.338.0")
	})
	h.stop(t)
	if snap.Image.Building != "" {
		t.Errorf("image status after the build = %+v, want nothing building", snap.Image)
	}
}

// When the agent starts with a class image to build, as after a restart
// interrupted a build, status says the image is building, not that there is none.
func TestClassImagesBuilding(t *testing.T) {
	hold := make(chan struct{})
	h := startAgent(t, map[string]*container{}, nil, func(c *config.Class, h *harness) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("ARG BASE\nFROM $BASE\n"), 0o644)
		c.Dockerfile, c.Image = filepath.Join(dir, "Dockerfile"), "boxrunner/class-boxrunner:local"
		h.docker.holdBuild = hold
	})
	var snap status.Snapshot
	waitFor(t, "the build in status", func() bool {
		var err error
		snap, err = status.Fetch(context.Background(), h.socket)
		return err == nil && snap.Image.Building != ""
	})
	if want := (status.ImageStatus{Building: runnerimage.Identity("2.338.0")}); snap.Image != want {
		t.Errorf("image status while building = %+v, want %+v", snap.Image, want)
	}
	var b strings.Builder
	status.Print(&b, snap, time.Now())
	if want := "  image building " + runnerimage.Identity("2.338.0") + " (latest none)  "; !strings.Contains(b.String(), want) {
		t.Errorf("status =\n%s\nwant it to contain %q", b.String(), want)
	}
	close(hold)
	waitFor(t, "the built image in status", func() bool {
		var err error
		snap, err = status.Fetch(context.Background(), h.socket)
		return err == nil && snap.Image.Built == runnerimage.Identity("2.338.0") && snap.Image.Building == ""
	})
	h.stop(t)
}

// The agent builds nothing when the tag points to another image than the one
// boxrunner's CI verified: jobs keep the current image, and status says why.
func TestRefusesARepointedTag(t *testing.T) {
	h := startAgent(t, map[string]*container{}, nil, func(_ *config.Class, h *harness) {
		h.github.runner = "2.339.0"
		h.github.list = `{"2.338.0":"` + digest338 + `","2.339.0":"` + digest339 + `"}`
		h.github.tags = map[string]string{"2.338.0": digest338, "2.339.0": "sha256:" + strings.Repeat("e", 64)}
	})
	var snap status.Snapshot
	waitFor(t, "the refusal in status", func() bool {
		var err error
		snap, err = status.Fetch(context.Background(), h.socket)
		return err == nil && snap.Image.Refused != ""
	})
	h.stop(t)
	snap.Image.Disk = 0
	want := status.ImageStatus{
		Built: runnerimage.Identity("2.338.0"), Latest: runnerimage.Identity("2.339.0"),
		Refused: "ghcr.io/actions/actions-runner:2.339.0 points to sha256:" + strings.Repeat("e", 64) + ", not to " + digest339 + " that boxrunner's CI verified",
	}
	if snap.Image != want {
		t.Errorf("image status = %+v, want %+v", snap.Image, want)
	}
	h.docker.mu.Lock()
	defer h.docker.mu.Unlock()
	if len(h.docker.builds) != 0 {
		t.Errorf("the agent pulled or built %q", h.docker.builds)
	}
}

func TestStatusVersions(t *testing.T) {
	h := startAgent(t, map[string]*container{}, nil)
	defer h.stop(t)
	var got [2]string
	waitFor(t, "the Worker's version and the latest release in the status", func() bool {
		s, err := status.Fetch(context.Background(), h.socket)
		got = [2]string{s.Worker, s.Latest}
		return err == nil && got == [2]string{"0.1.0", "0.2.2"}
	})
}

func TestMetrics(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := netip.MustParseAddrPort(l.Addr().String())
	l.Close()
	h := startAgent(t, map[string]*container{}, nil, func(_ *config.Class, h *harness) { h.metrics = addr })
	defer h.stop(t)
	h.github.queue <- message(1, 1, `{"messageType":"JobAvailable","runnerRequestId":501,"queueTime":"2026-10-13T21:00:00Z"}`)
	var name string
	waitFor(t, "a running job container", func() bool {
		for n, c := range h.docker.snapshot() {
			name = n
			return c.State == "running"
		}
		return false
	})
	h.github.queue <- message(2, 1, fmt.Sprintf(`{"messageType":"JobStarted","runnerRequestId":501,"runnerId":41,"runnerName":%q}`, name))
	h.github.queue <- message(3, 0, fmt.Sprintf(`{"messageType":"JobCompleted","runnerRequestId":501,"runnerId":41,"runnerName":%q,"result":"succeeded","jobDisplayName":"test",
		"queueTime":"2026-10-13T21:00:00Z","runnerAssignTime":"2026-10-13T21:00:08Z","finishTime":"2026-10-13T21:03:00Z"}`, name))

	want := []string{
		`boxrunner_agent_info{box="box-1",version="0.1.0"} 1`,
		`gha_completed_jobs_total{name="boxrunner",organization="octo",repository="site",job_result="succeeded"} 1`,
		`gha_job_execution_duration_seconds_sum{name="boxrunner",organization="octo",repository="site",job_result="succeeded"} 172`,
		`boxrunner_job_queue_duration_seconds_sum{name="boxrunner",organization="octo",repository="site",job_result="succeeded"} 8`,
		`boxrunner_class_max_runners{class="boxrunner"} 1`,
		`boxrunner_draining 0`,
	}
	var body string
	defer func() {
		if t.Failed() {
			t.Logf("the last metrics:\n%s", body)
		}
	}()
	waitFor(t, "the finished job and a heartbeat in the metrics", func() bool {
		resp, err := http.Get("http://" + addr.String() + "/metrics")
		if err != nil {
			return false
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		body = string(b)
		lines := strings.Split(body, "\n")
		for _, line := range want {
			if !slices.Contains(lines, line) {
				return false
			}
		}
		return !slices.Contains(lines, "boxrunner_heartbeat_last_success_timestamp_seconds 0")
	})
}

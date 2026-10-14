package docker

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type seen struct {
	method, uri, contentType string
	body                     any
}

// fakeDaemon serves the Engine API on a unix socket. route answers a request
// by its method and path; /version answers with apiVersion.
func fakeDaemon(t *testing.T, apiVersion string, route func(w http.ResponseWriter, r *http.Request)) (*Client, *[]seen) {
	// A unix socket path must stay under 104 bytes on macOS, which a long
	// test name in t.TempDir() can exceed.
	dir, err := os.MkdirTemp("", "d")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "docker.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var requests []seen
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			io.WriteString(w, `{"ApiVersion":"`+apiVersion+`","Version":"27.5.1"}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body any
		if json.Unmarshal(raw, &body) != nil && len(raw) > 0 {
			body = len(raw)
		}
		requests = append(requests, seen{r.Method, r.URL.RequestURI(), r.Header.Get("Content-Type"), body})
		route(w, r)
	}))
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	c, err := Dial(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	return c, &requests
}

func ok(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, `{"Id":"abc"}`) }

func TestNegotiatesVersion(t *testing.T) {
	for daemon, want := range map[string]string{"1.41": "1.41", "1.45": "1.45", "1.52": "1.47"} {
		c, requests := fakeDaemon(t, daemon, ok)
		if err := c.Start(context.Background(), "abc"); err != nil {
			t.Fatal(err)
		}
		if c.API != want || (*requests)[0].uri != "/v"+want+"/containers/abc/start" {
			t.Errorf("daemon %s: API %s, request %s", daemon, c.API, (*requests)[0].uri)
		}
	}
}

func TestCreateShape(t *testing.T) {
	c, requests := fakeDaemon(t, "1.47", ok)
	id, err := c.Create(context.Background(), ContainerSpec{
		Name:         "box-1-boxrunner-light-3f9a2c",
		Image:        "boxrunner/runner:local",
		Env:          []string{"ACTIONS_RUNNER_INPUT_JITCONFIG=eyIu"},
		Labels:       map[string]string{"boxrunner.box": "box-1", "boxrunner.runner": "box-1-boxrunner-light-3f9a2c"},
		NanoCPUs:     4e9,
		Memory:       2 << 30,
		PidsLimit:    4096,
		ShmSize:      1 << 30,
		CgroupParent: "boxrunner_idle.slice",
		Network:      "boxrunner",
		Runtime:      "sysbox-runc",
	})
	if err != nil || id != "abc" {
		t.Fatalf("Create = %q, %v", id, err)
	}
	var want any
	json.Unmarshal([]byte(`{
		"Image": "boxrunner/runner:local",
		"Env": ["ACTIONS_RUNNER_INPUT_JITCONFIG=eyIu"],
		"Labels": {"boxrunner.box": "box-1", "boxrunner.runner": "box-1-boxrunner-light-3f9a2c"},
		"HostConfig": {
			"NanoCpus": 4000000000,
			"Memory": 2147483648,
			"MemorySwap": 2147483648,
			"PidsLimit": 4096,
			"ShmSize": 1073741824,
			"CgroupParent": "boxrunner_idle.slice",
			"NetworkMode": "boxrunner",
			"Runtime": "sysbox-runc"
		}
	}`), &want)
	got := (*requests)[0]
	if got.method != "POST" || got.uri != "/v1.47/containers/create?name=box-1-boxrunner-light-3f9a2c" || got.contentType != "application/json" || !reflect.DeepEqual(got.body, want) {
		t.Errorf("request = %+v", got)
	}
}

func TestPullByDigestReportsError(t *testing.T) {
	c, requests := fakeDaemon(t, "1.47", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"status":"Pulling from library/registry"}`+"\n"+`{"error":"toomanyrequests: rate limit"}`+"\n")
	})
	err := c.Pull(context.Background(), "registry@sha256:a3d8")
	if err == nil || err.Error() != "docker pull: toomanyrequests: rate limit" {
		t.Errorf("Pull = %v", err)
	}
	if got := (*requests)[0]; got.method != "POST" || got.uri != "/v1.47/images/create?fromImage=registry%40sha256%3Aa3d8" {
		t.Errorf("request = %+v", got)
	}
}

func TestNetworkGateway(t *testing.T) {
	c, _ := fakeDaemon(t, "1.47", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"Name":"boxrunner","IPAM":{"Config":[{"Subnet":"fd00:1::/64","Gateway":"fd00:1::1"},{"Subnet":"172.18.0.0/16","Gateway":"172.18.0.1"}]}}`)
	})
	n, ok, err := c.Network(context.Background(), "boxrunner")
	gateway, found := n.Gateway()
	if err != nil || !ok || !found || gateway.String() != "172.18.0.1" {
		t.Errorf("Gateway = %v, %v (network %v, %v)", gateway, found, ok, err)
	}
	if _, found := (Network{}).Gateway(); found {
		t.Error("a network without IPAM has a gateway")
	}
}

func TestListAndRemove(t *testing.T) {
	c, requests := fakeDaemon(t, "1.47", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			io.WriteString(w, `[{"Id":"abc","Names":["/r1"],"Labels":{"boxrunner.runner":"r1"},"State":"running","Created":1792000000,"SizeRw":42}]`)
		case http.MethodDelete:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"No such container: abc"}`)
		}
	})
	got, err := c.List(context.Background(), "boxrunner.box=box-1", true)
	want := []Container{{ID: "abc", Names: []string{"/r1"}, Labels: map[string]string{"boxrunner.runner": "r1"}, State: "running", Created: 1792000000, SizeRw: 42}}
	if err != nil || !reflect.DeepEqual(got, want) || !got[0].Running() {
		t.Errorf("List = %+v, %v", got, err)
	}
	if err := c.Remove(context.Background(), "abc"); err != nil {
		t.Errorf("Remove of a missing container = %v", err)
	}
	wantURIs := []string{
		"/v1.47/containers/json?all=1&filters=%7B%22label%22%3A%5B%22boxrunner.box%3Dbox-1%22%5D%7D&size=1",
		"/v1.47/containers/abc?force=1&v=1",
	}
	if (*requests)[0].uri != wantURIs[0] || (*requests)[1].uri != wantURIs[1] {
		t.Errorf("requests = %+v", *requests)
	}
}

func TestCreateNetworkShape(t *testing.T) {
	c, requests := fakeDaemon(t, "1.47", ok)
	err := c.CreateNetwork(context.Background(), NetworkSpec{Name: "boxrunner", Bridge: "boxrunner0", Labels: map[string]string{"boxrunner.box": "box-1"}})
	if err != nil {
		t.Fatal(err)
	}
	var want any
	json.Unmarshal([]byte(`{"Name":"boxrunner","Driver":"bridge","Labels":{"boxrunner.box":"box-1"},
		"Options":{"com.docker.network.bridge.name":"boxrunner0","com.docker.network.bridge.enable_icc":"false"}}`), &want)
	if got := (*requests)[0]; got.uri != "/v1.47/networks/create" || !reflect.DeepEqual(got.body, want) {
		t.Errorf("request = %+v", got)
	}
}

func TestBuildReportsError(t *testing.T) {
	c, requests := fakeDaemon(t, "1.47", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"stream":"Step 1/9 : FROM ghcr.io/actions/actions-runner:2.329.0"}`+"\n")
		io.WriteString(w, `{"errorDetail":{"message":"manifest unknown"},"error":"manifest unknown"}`+"\n")
	})
	err := c.Build(context.Background(), strings.NewReader("tar bytes"), BuildOptions{Tag: "boxrunner/runner:local", Args: map[string]string{"RUNNER_VERSION": "2.329.0"}, Pull: true})
	if err == nil || err.Error() != "docker build: manifest unknown" {
		t.Errorf("Build = %v", err)
	}
	got := (*requests)[0]
	wantURI := "/v1.47/build?buildargs=%7B%22RUNNER_VERSION%22%3A%222.329.0%22%7D&forcerm=1&pull=1&rm=1&t=boxrunner%2Frunner%3Alocal"
	if got.uri != wantURI || got.contentType != "application/x-tar" || got.body != 9 {
		t.Errorf("request = %+v", got)
	}
}

func TestBuildFromLocalBaseWithLabels(t *testing.T) {
	c, requests := fakeDaemon(t, "1.47", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"stream":"Successfully built"}`+"\n")
	})
	err := c.Build(context.Background(), strings.NewReader("tar"), BuildOptions{
		Tag:    "boxrunner/class-web:local",
		Args:   map[string]string{"BASE": "boxrunner/runner:local"},
		Labels: map[string]string{"boxrunner.base": "sha256:abc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantURI := "/v1.47/build?buildargs=%7B%22BASE%22%3A%22boxrunner%2Frunner%3Alocal%22%7D&forcerm=1&labels=%7B%22boxrunner.base%22%3A%22sha256%3Aabc%22%7D&rm=1&t=boxrunner%2Fclass-web%3Alocal"
	if got := (*requests)[0]; got.uri != wantURI {
		t.Errorf("uri = %s", got.uri)
	}
}

func TestInfoReadsRuntimesAndPools(t *testing.T) {
	c, _ := fakeDaemon(t, "1.47", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"ServerVersion":"29.1.3","CgroupDriver":"systemd",
			"Runtimes":{"runc":{"path":"runc"},"sysbox-runc":{"path":"/usr/bin/sysbox-runc"}},
			"DefaultAddressPools":[{"Base":"172.17.0.0/16","Size":16},{"Base":"192.168.0.0/16","Size":20}]}`)
	})
	got, err := c.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := Info{
		ServerVersion: "29.1.3", CgroupDriver: "systemd",
		Runtimes:            map[string]struct{}{"runc": {}, "sysbox-runc": {}},
		DefaultAddressPools: []AddressPool{{"172.17.0.0/16", 16}, {"192.168.0.0/16", 20}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Info =\n%+v\nwant\n%+v", got, want)
	}
}

func TestTailReadsFrames(t *testing.T) {
	frame := func(stream byte, s string) string {
		return string([]byte{stream, 0, 0, 0, 0, 0, 0, byte(len(s))}) + s
	}
	c, requests := fakeDaemon(t, "1.47", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, frame(1, "Connected to GitHub\n")+frame(2, "exec /entrypoint.sh: no such file or directory\n"))
	})
	got, err := c.Tail(context.Background(), "box-1-web-3f9a2c", 20)
	want := []string{"Connected to GitHub", "exec /entrypoint.sh: no such file or directory"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("Tail = %q, %v", got, err)
	}
	if uri := (*requests)[0].uri; uri != "/v1.47/containers/box-1-web-3f9a2c/logs?stderr=1&stdout=1&tail=20" {
		t.Errorf("uri = %s", uri)
	}
}

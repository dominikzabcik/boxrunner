package runnerimage

import (
	"archive/tar"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseLatest(t *testing.T) {
	got, err := ParseLatest("https://github.com/actions/runner/releases/tag/v2.329.0")
	if err != nil || got != "2.329.0" {
		t.Errorf("ParseLatest = %q, %v", got, err)
	}
	if _, err := ParseLatest("https://github.com/login"); err == nil || err.Error() != `latest runner release: unexpected redirect "https://github.com/login"` {
		t.Errorf("ParseLatest of a login page = %v", err)
	}
}

func TestLatestFollowsNoRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("method %s", r.Method)
		}
		http.Redirect(w, r, "/actions/runner/releases/tag/v2.330.1", http.StatusFound)
	}))
	defer srv.Close()
	got, err := Latest(context.Background(), srv.Client(), srv.URL+"/actions/runner/releases/latest")
	if err != nil || got != "2.330.1" {
		t.Errorf("Latest = %q, %v", got, err)
	}
}

func TestTarHoldsTheContext(t *testing.T) {
	buf, err := Tar()
	if err != nil {
		t.Fatal(err)
	}
	r := tar.NewReader(buf)
	var got []string
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(r)
		got = append(got, h.Name+" "+map[bool]string{true: "exec", false: "file"}[h.Mode == 0o755])
		if h.Name == "Dockerfile" && !strings.Contains(string(body), "FROM ghcr.io/actions/actions-runner:${RUNNER_VERSION}") {
			t.Errorf("Dockerfile does not build from the official runner image")
		}
	}
	if strings.Join(got, ",") != "Dockerfile file,docker exec" {
		t.Errorf("tar = %v", got)
	}
}

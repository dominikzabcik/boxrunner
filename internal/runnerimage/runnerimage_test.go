package runnerimage

import (
	"archive/tar"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dominikzabcik/boxrunner/internal/docker"
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
	if strings.Join(got, ",") != "Dockerfile file,docker exec,entrypoint.sh exec" {
		t.Errorf("tar = %v", got)
	}
}

func TestDirTarHashFollowsContent(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Dockerfile", "ARG BASE\nFROM ${BASE}\n")
	write("setup.sh", "echo hi\n")
	_, first, err := DirTar(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, again, _ := DirTar(dir); again != first {
		t.Errorf("same files gave %s then %s", first, again)
	}
	write("setup.sh", "echo bye\n")
	if _, changed, _ := DirTar(dir); changed == first {
		t.Error("a changed file kept the hash")
	}
}

// Three rebuilds' images as Docker lists them: runner 2.338.0 is current, a
// kept failed job's container still uses the 2.337.0 class image, and nothing
// uses 2.336.0's. Docker does not list the untagged job images the old class
// images were built from.
func rebuilds() []docker.ImageSummary {
	runner := func(id, version string, size int64) docker.ImageSummary {
		return docker.ImageSummary{ID: id, RepoTags: []string{"ghcr.io/actions/actions-runner:" + version}, Size: size}
	}
	class := func(id, version, base string, size int64, tags ...string) docker.ImageSummary {
		return docker.ImageSummary{ID: id, RepoTags: tags, Labels: map[string]string{"boxrunner.image": version + "+3f9a2c0b1d4e", "boxrunner.base": base}, Size: size}
	}
	return []docker.ImageSummary{
		runner("sha256:b338", "2.338.0", 1000),
		runner("sha256:b337", "2.337.0", 990),
		runner("sha256:b336", "2.336.0", 980),
		{ID: "sha256:j338", RepoTags: []string{"boxrunner/runner:local"}, Labels: map[string]string{"boxrunner.image": "2.338.0+3f9a2c0b1d4e"}, Size: 1500},
		class("sha256:w338", "2.338.0", "sha256:j338", 2500, "boxrunner/class-web:local"),
		class("sha256:w337", "2.337.0", "sha256:j337", 2490),
		class("sha256:w336", "2.336.0", "sha256:j336", 2480, "<none>:<none>"),
		{ID: "sha256:a1", RepoTags: []string{}, Size: 8},
		{ID: "sha256:a2", RepoTags: []string{"alpine:3.20"}, Size: 9},
		{ID: "sha256:a3", RepoTags: []string{"ghcr.io/actions/actions-runner:latest"}, Size: 1310},
	}
}

func TestSuperseded(t *testing.T) {
	kept := []docker.Container{{ImageID: "sha256:w337"}, {ImageID: "sha256:a1"}}
	want := []string{"sha256:w336", "ghcr.io/actions/actions-runner:2.336.0", "ghcr.io/actions/actions-runner:2.337.0"}
	if got := Superseded(rebuilds(), kept, "2.338.0"); !reflect.DeepEqual(got, want) {
		t.Errorf("Superseded with a kept container = %q, want %q", got, want)
	}
	want = []string{"sha256:w336", "sha256:w337", "ghcr.io/actions/actions-runner:2.336.0", "ghcr.io/actions/actions-runner:2.337.0"}
	if got := Superseded(rebuilds(), nil, "2.338.0"); !reflect.DeepEqual(got, want) {
		t.Errorf("Superseded without containers = %q, want %q", got, want)
	}
	// boxrunner shell commits the kept container, labels and all, and removes
	// the copy when the shell exits; before its container starts, nothing uses it.
	inspect := docker.ImageSummary{ID: "sha256:i337", RepoTags: []string{"boxrunner/inspect:box-1-web-3f9a2c"}, Labels: map[string]string{"boxrunner.image": "2.337.0+3f9a2c0b1d4e", "boxrunner.base": "sha256:j337"}, Size: 2600}
	want = []string{"sha256:w336", "ghcr.io/actions/actions-runner:2.336.0", "ghcr.io/actions/actions-runner:2.337.0"}
	if got := Superseded(append(rebuilds(), inspect), kept, "2.338.0"); !reflect.DeepEqual(got, want) {
		t.Errorf("Superseded while boxrunner shell opens a copy = %q, want %q", got, want)
	}
	noClasses := append(rebuilds()[:4], docker.ImageSummary{ID: "sha256:j337", Labels: map[string]string{"boxrunner.image": "2.337.0+3f9a2c0b1d4e"}, Size: 1490})
	want = []string{"sha256:j337", "ghcr.io/actions/actions-runner:2.336.0", "ghcr.io/actions/actions-runner:2.337.0"}
	if got := Superseded(noClasses, []docker.Container{{ImageID: "sha256:j338"}}, "2.338.0"); !reflect.DeepEqual(got, want) {
		t.Errorf("Superseded of a box without class images = %q, want %q", got, want)
	}
}

// Each image's size includes the image it was built from, so the sum of the
// sizes would count the runner image's layers several times.
func TestDiskUse(t *testing.T) {
	if got := DiskUse(rebuilds()); got != 7470 {
		t.Errorf("DiskUse = %d, want 7470", got)
	}
	all := rebuilds()
	if got := DiskUse([]docker.ImageSummary{all[0], all[3], all[4]}); got != 2500 {
		t.Errorf("DiskUse of one rebuild's images = %d, want 2500", got)
	}
}

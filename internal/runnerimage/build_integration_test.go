package runnerimage

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/docker"
	"github.com/dominikzabcik/boxrunner/internal/layout"
	"github.com/dominikzabcik/boxrunner/internal/runnerdigests"
)

// TestBuildAgainstDocker builds the real job image with the latest runner and
// checks what jobs rely on. It needs a Docker daemon and the network, so it
// runs only with BOXRUNNER_DOCKER_SOCKET set, and removes the image after.
func TestBuildAgainstDocker(t *testing.T) {
	socket := os.Getenv("BOXRUNNER_DOCKER_SOCKET")
	if socket == "" {
		t.Skip("set BOXRUNNER_DOCKER_SOCKET to build the image against a real daemon")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	d, err := docker.Dial(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	// BOXRUNNER_RUNNER_DIGESTS can name a list of your own, such as one
	// scripts/runner-digests.sh wrote, served over HTTP.
	runner := runnerdigests.Source{List: os.Getenv("BOXRUNNER_RUNNER_DIGESTS")}
	choice, err := runner.Choose(ctx)
	if err != nil {
		t.Fatal(err)
	}
	identity := Identity(choice.Version)
	if err := Build(ctx, d, runner, identity, choice.Digest); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.RemoveImage(context.Background(), layout.Image) })
	if got, err := Built(ctx, d); err != nil || got != (JobImage{identity, choice.Digest}) {
		t.Errorf("Built = %+v, %v; want %s from %s", got, err, identity, choice.Digest)
	}

	script := `docker ps; echo "docker exit $?"
id -un
touch "$RUNNER_TOOL_CACHE/x" && echo "tool cache writable"
for tool in zstd gcc make python3 git curl jq; do command -v $tool >/dev/null || echo "missing $tool"; done
python3 -m venv /tmp/venv && echo "venv ok"
test -x /home/runner/run.sh && echo "run.sh ok"`
	id, err := d.Create(ctx, docker.ContainerSpec{Name: "boxrunner-image-test", Image: layout.Image, Cmd: []string{"bash", "-c", script}, Tty: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Remove(context.Background(), id)
	if err := d.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Wait(ctx, id); err != nil {
		t.Fatal(err)
	}
	out, _ := d.Logs(ctx, id)
	out = strings.ReplaceAll(out, "\r", "")
	want := `boxrunner: this job's class has no Docker daemon.
boxrunner: services:, container: jobs, Docker container actions and docker build need a class with docker = true (docs/config.md).
docker exit 1
runner
tool cache writable
venv ok
run.sh ok
`
	if out != want {
		t.Errorf("container output =\n%s\nwant\n%s", out, want)
	}
}

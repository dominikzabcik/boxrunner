package mirror_test

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/agent"
	"github.com/dominikzabcik/boxrunner/internal/config"
	"github.com/dominikzabcik/boxrunner/internal/docker"
	"github.com/dominikzabcik/boxrunner/internal/layout"
	"github.com/dominikzabcik/boxrunner/internal/mirror"
	"github.com/dominikzabcik/boxrunner/internal/scheduler"
)

// TestSecondJobPullsFromTheMirror starts two job containers the way the agent
// does, one after the other, and pulls the same image in each. The second
// pull must reach the mirror and be served from its cache, without a fetch
// from Docker Hub. It needs a box where install ran with a docker class, so
// it runs only with BOXRUNNER_DOCKER_SOCKET set.
func TestSecondJobPullsFromTheMirror(t *testing.T) {
	socket := os.Getenv("BOXRUNNER_DOCKER_SOCKET")
	if socket == "" {
		t.Skip("set BOXRUNNER_DOCKER_SOCKET on a box installed with a docker class")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	d, err := docker.Dial(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	addr, err := mirror.Find(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if err := mirror.Ensure(ctx, d, addr); err != nil {
		t.Fatal(err)
	}

	class := config.Class{Name: "t", NanoCPUs: 2e9, Memory: 3 << 30, Pids: 4096, Shm: 1 << 30, Priority: config.Idle, Image: layout.Image, Docker: true}
	pull := func(name string) (time.Duration, string) {
		started := time.Now()
		spec := agent.Spec("box-1", class, scheduler.Runner{Name: name, ScaleSet: scheduler.Key{Repo: "octo/site", Class: "t"}}, "unused", mirror.URL(addr), "")
		spec.Labels = nil
		spec.Tty = true
		spec.Cmd = []string{"bash", "-c", `until docker info >/dev/null 2>&1; do sleep 1; done
start=$(date +%s%N); docker pull -q busybox:1.37 >/dev/null && echo "pulled in $(( ($(date +%s%N) - start) / 1000000 )) ms"`}
		d.Remove(ctx, name)
		id, err := d.Create(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Remove(context.WithoutCancel(ctx), id)
		if err := d.Start(ctx, id); err != nil {
			t.Fatal(err)
		}
		if code, err := d.Wait(ctx, id); err != nil || code != 0 {
			out, _ := d.Logs(ctx, id)
			t.Fatalf("%s exited %d, %v:\n%s", name, code, err, out)
		}
		out, _ := d.Logs(ctx, id)
		var ms int
		fmt.Sscanf(strings.TrimSpace(out), "pulled in %d ms", &ms)
		log, err := d.LogsSince(ctx, layout.Mirror, started)
		if err != nil {
			t.Fatal(err)
		}
		return time.Duration(ms) * time.Millisecond, log
	}

	first, _ := pull("boxrunner-test-first")
	second, log := pull("boxrunner-test-second")
	t.Logf("first pull %v, second pull %v", first, second)
	served := regexp.MustCompile(`http.request.method=GET .*http.request.uri="/v2/library/busybox/blobs/sha256:[0-9a-f]+[^"]*".*http.response.status=200`)
	if !served.MatchString(log) {
		t.Errorf("the mirror served no busybox blob to the second job; its log:\n%s", log)
	}
	if strings.Contains(log, "Adding new scheduler entry") {
		t.Errorf("the mirror fetched from Docker Hub for the second job; its log:\n%s", log)
	}
}

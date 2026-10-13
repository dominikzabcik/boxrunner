package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/docker"
	"github.com/dominikzabcik/boxrunner/internal/firewall"
	"github.com/dominikzabcik/boxrunner/internal/layout"
	"github.com/dominikzabcik/boxrunner/internal/runnerimage"
)

// Exec runs a command and returns its combined output.
func Exec(ctx context.Context, stdin, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

var managedPaths = []string{
	layout.Binary, layout.IdleSlicePath, layout.UnitPath, layout.ConfigPath, layout.TokenPath,
	layout.ConfigDir, layout.StateDir,
}

// Observe looks at the box. It returns a Docker client when the daemon answers.
func Observe(ctx context.Context, run firewall.Run) (Observed, *docker.Client) {
	o := Observed{Paths: map[string]File{}}
	if release, err := os.ReadFile("/etc/os-release"); err == nil {
		o.Debian = debianLike(string(release))
	}
	_, errDockerd := exec.LookPath("dockerd")
	_, errDocker := exec.LookPath("docker")
	o.DockerBinary = errDockerd == nil || errDocker == nil
	_, err := os.Stat("/sys/fs/cgroup/cgroup.controllers")
	o.CgroupV2 = err == nil

	for _, p := range managedPaths {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		f := File{Mode: info.Mode()}
		if info.Mode().IsRegular() {
			f.Content, _ = os.ReadFile(p)
		}
		o.Paths[p] = f
	}

	o.Firewall = firewall.Check(ctx, run, layout.Bridge)
	_, err = run(ctx, "", "systemctl", "is-enabled", "--quiet", layout.UnitName)
	o.UnitEnabled = err == nil
	_, err = run(ctx, "", "systemctl", "is-active", "--quiet", layout.UnitName)
	o.UnitActive = err == nil

	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	d, err := docker.Dial(dctx, layout.DockerSocket)
	if err != nil {
		return o, nil
	}
	o.DockerUp = true
	if info, err := d.Info(ctx); err == nil {
		o.CgroupDriver = info.CgroupDriver
	}
	if n, ok, err := d.Network(ctx, layout.Network); err == nil && ok {
		o.Network = Network{Exists: true}
		if n.Driver != "bridge" || n.Options["com.docker.network.bridge.name"] != layout.Bridge || n.Options["com.docker.network.bridge.enable_icc"] != "false" {
			o.Network.Problem = fmt.Sprintf("a Docker network named %s exists but is not boxrunner's (driver %q, bridge %q, icc %q); remove it with `docker network rm %s` and run install again",
				layout.Network, n.Driver, n.Options["com.docker.network.bridge.name"], n.Options["com.docker.network.bridge.enable_icc"], layout.Network)
		}
	}
	_, o.Image, _ = d.ImageLabels(ctx, layout.Image)
	if list, err := d.List(ctx, layout.LabelBox, false); err == nil {
		o.Containers = len(list)
	}
	return o, d
}

func debianLike(osRelease string) bool {
	for _, line := range strings.Split(osRelease, "\n") {
		key, value, _ := strings.Cut(line, "=")
		if key != "ID" && key != "ID_LIKE" {
			continue
		}
		for _, id := range strings.Fields(strings.Trim(value, `"'`)) {
			if id == "debian" || id == "ubuntu" {
				return true
			}
		}
	}
	return false
}

// Execute runs steps in order and stops at the first failure.
func Execute(ctx context.Context, steps []Step, run firewall.Run, d *docker.Client, out io.Writer) error {
	for _, step := range steps {
		fmt.Fprintf(out, "==> %s\n", step)
		var err error
		d, err = execute(ctx, step, run, d)
		if err != nil {
			return fmt.Errorf("%s: %w", step, err)
		}
	}
	return nil
}

func execute(ctx context.Context, step Step, run firewall.Run, d *docker.Client) (*docker.Client, error) {
	command := func(name string, args ...string) error {
		if out, err := run(ctx, "", name, args...); err != nil {
			return fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, out)
		}
		return nil
	}
	needDocker := func() error {
		if d == nil {
			return errors.New("docker is not running")
		}
		return nil
	}
	switch s := step.(type) {
	case InstallDocker:
		if s.Package {
			if err := command("apt-get", "update", "-qq"); err != nil {
				return d, err
			}
			if err := command("env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "install", "-y", "-qq", "docker.io"); err != nil {
				return d, err
			}
		}
		if err := command("systemctl", "enable", "--now", "docker.service"); err != nil {
			return d, err
		}
		for range 30 {
			if c, err := docker.Dial(ctx, layout.DockerSocket); err == nil {
				return c, nil
			}
			time.Sleep(time.Second)
		}
		return d, errors.New("docker did not answer within 30 seconds")
	case WriteFile:
		return d, writeFile(s)
	case DaemonReload:
		return d, command("systemctl", "daemon-reload")
	case CreateNetwork:
		if err := needDocker(); err != nil {
			return d, err
		}
		return d, d.CreateNetwork(ctx, docker.NetworkSpec{Name: layout.Network, Bridge: layout.Bridge})
	case ApplyFirewall:
		return d, firewall.Apply(ctx, run, layout.Bridge)
	case BuildImage:
		if err := needDocker(); err != nil {
			return d, err
		}
		version, err := runnerimage.Latest(ctx, &http.Client{Timeout: 30 * time.Second}, runnerimage.ReleasesLatest)
		if err != nil {
			return d, err
		}
		return d, runnerimage.Build(ctx, d, version)
	case EnableUnit:
		return d, command("systemctl", "enable", "--now", layout.UnitName)
	case RestartUnit:
		return d, command("systemctl", "restart", layout.UnitName)
	case StopUnit:
		return d, command("systemctl", "disable", "--now", layout.UnitName)
	case RemoveContainers:
		if err := needDocker(); err != nil {
			return d, err
		}
		list, err := d.List(ctx, layout.LabelBox, false)
		if err != nil {
			return d, err
		}
		for _, c := range list {
			if err := d.Remove(ctx, c.ID); err != nil {
				return d, err
			}
		}
		return d, nil
	case RemoveNetwork:
		if err := needDocker(); err != nil {
			return d, err
		}
		return d, d.RemoveNetwork(ctx, layout.Network)
	case RemoveImage:
		if err := needDocker(); err != nil {
			return d, err
		}
		return d, d.RemoveImage(ctx, layout.Image)
	case RemoveFirewall:
		return d, firewall.Remove(ctx, run, layout.Bridge)
	case RemovePath:
		return d, os.RemoveAll(s.Path)
	case Blocked:
		return d, errors.New(s.Reason)
	default:
		panic(fmt.Sprintf("install: unhandled step %T", step))
	}
}

// writeFile replaces a file atomically with the right mode from the start, so
// a token is never readable by others, not even briefly.
func writeFile(s WriteFile) error {
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(s.Path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(s.Mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(s.Content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.Path)
}

const rounds = 3

// Install converges the box: observe, plan, execute, until a plan comes back
// empty. It returns every step it ran; none means the box was already done.
func Install(ctx context.Context, d Desired, run firewall.Run, out io.Writer) ([]Step, error) {
	var done []Step
	for range rounds {
		o, client := Observe(ctx, run)
		steps := Plan(d, o)
		if len(steps) == 0 {
			return done, nil
		}
		if reasons := blockedReasons(steps); reasons != nil {
			return done, errors.New(strings.Join(reasons, "\n"))
		}
		if err := Execute(ctx, steps, run, client, out); err != nil {
			return done, err
		}
		done = append(done, steps...)
	}
	o, _ := Observe(ctx, run)
	return done, fmt.Errorf("the box did not settle; still to do: %v", Plan(d, o))
}

// Uninstall removes everything install created.
func Uninstall(ctx context.Context, run firewall.Run, out io.Writer) ([]Step, error) {
	o, client := Observe(ctx, run)
	steps := PlanUninstall(o)
	return steps, Execute(ctx, steps, run, client, out)
}

func blockedReasons(steps []Step) []string {
	var reasons []string
	for _, s := range steps {
		if b, ok := s.(Blocked); ok {
			reasons = append(reasons, b.Reason)
		}
	}
	return reasons
}

// HostMemory reads the box's memory from /proc/meminfo.
func HostMemory() (int64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			var kib int64
			if _, err := fmt.Sscanf(strings.TrimSpace(rest), "%d kB", &kib); err != nil {
				return 0, err
			}
			return kib * 1024, nil
		}
	}
	return 0, errors.New("/proc/meminfo has no MemTotal")
}

package install

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/docker"
	"github.com/dominikzabcik/boxrunner/internal/firewall"
	"github.com/dominikzabcik/boxrunner/internal/layout"
	"github.com/dominikzabcik/boxrunner/internal/mirror"
	"github.com/dominikzabcik/boxrunner/internal/runnerdigests"
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
	layout.ConfigDir, layout.StateDir, layout.Socket,
}

// Observe looks at the box. It returns a Docker client when the daemon answers.
func Observe(ctx context.Context, run firewall.Run) (Observed, *docker.Client) {
	o := Observed{Paths: map[string]File{}}
	if release, err := os.ReadFile("/etc/os-release"); err == nil {
		o.Debian = debianLike(string(release))
		o.Distro, o.DistroVersion = osRelease(string(release))
	}
	if kernel, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		o.Kernel = strings.TrimSpace(string(kernel))
	}
	o.Arch = runtime.GOARCH
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

	o.Firewall = firewall.Check(ctx, run, firewall.Rules{Bridge: layout.Bridge})
	o.FirewallMirror = firewall.Stale
	_, err = run(ctx, "", "systemctl", "is-enabled", "--quiet", layout.UnitName)
	o.UnitEnabled = err == nil
	_, err = run(ctx, "", "systemctl", "is-active", "--quiet", layout.UnitName)
	o.UnitActive = err == nil
	if pid, err := run(ctx, "", "systemctl", "show", "--property=MainPID", "--value", layout.UnitName); err == nil {
		exe, err := os.Readlink("/proc/" + strings.TrimSpace(pid) + "/exe")
		o.AgentReplaced = err == nil && strings.HasSuffix(exe, " (deleted)")
	}

	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	d, err := docker.Dial(dctx, layout.DockerSocket)
	if err != nil {
		return o, nil
	}
	o.DockerUp = true
	if info, err := d.Info(ctx); err == nil {
		o.CgroupDriver = info.CgroupDriver
		_, o.Sysbox = info.Runtimes[layout.SysboxRuntime]
	}
	if n, ok, err := d.Network(ctx, layout.Network); err == nil && ok {
		o.Network = Network{Exists: true}
		if gateway, ok := n.Gateway(); ok {
			o.Network.Gateway = gateway
			o.FirewallMirror = firewall.Check(ctx, run, firewall.Rules{Bridge: layout.Bridge, Mirror: mirror.Addr(gateway)})
		}
		if n.Driver != "bridge" || n.Options["com.docker.network.bridge.name"] != layout.Bridge || n.Options["com.docker.network.bridge.enable_icc"] != "false" {
			o.Network.Problem = fmt.Sprintf("a Docker network named %s exists but is not boxrunner's (driver %q, bridge %q, icc %q); remove it with `docker network rm %s` and run install again",
				layout.Network, n.Driver, n.Options["com.docker.network.bridge.name"], n.Options["com.docker.network.bridge.enable_icc"], layout.Network)
		}
	}
	o.Mirror, _ = mirror.Observe(ctx, d)
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

// osRelease returns ID and VERSION_ID from /etc/os-release.
func osRelease(content string) (id, version string) {
	for _, line := range strings.Split(content, "\n") {
		key, value, _ := strings.Cut(line, "=")
		value = strings.Trim(value, `"'`)
		switch key {
		case "ID":
			id = value
		case "VERSION_ID":
			version = value
		}
	}
	return id, version
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
		return waitForDocker(ctx, "")
	case InstallSysbox:
		if err := needDocker(); err != nil {
			return d, err
		}
		if err := pinNetworks(ctx, d); err != nil {
			return d, err
		}
		deb, err := download(ctx, s.Package)
		if err != nil {
			return d, err
		}
		defer os.RemoveAll(filepath.Dir(deb))
		if err := command("apt-get", "update", "-qq"); err != nil {
			return d, err
		}
		// This also finishes a package left half configured by an earlier run.
		if err := command("env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "install", "-y", "-qq", deb); err != nil {
			return d, err
		}
		return waitForDocker(ctx, layout.SysboxRuntime)
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
		rules := firewall.Rules{Bridge: layout.Bridge}
		if s.Mirror {
			if err := needDocker(); err != nil {
				return d, err
			}
			addr, err := mirror.Find(ctx, d)
			if err != nil {
				return d, err
			}
			rules.Mirror = addr
		}
		return d, firewall.Apply(ctx, run, rules)
	case StartMirror:
		if err := needDocker(); err != nil {
			return d, err
		}
		addr, err := mirror.Find(ctx, d)
		if err != nil {
			return d, err
		}
		return d, mirror.Ensure(ctx, d, addr)
	case RemoveMirror:
		if err := needDocker(); err != nil {
			return d, err
		}
		return d, mirror.Remove(ctx, d)
	case BuildImage:
		if err := needDocker(); err != nil {
			return d, err
		}
		var runner runnerdigests.Source
		choice, err := runner.Choose(ctx)
		if err != nil {
			return d, err
		}
		return d, runnerimage.Build(ctx, d, runner, runnerimage.Identity(choice.Version), choice.Digest)
	case EnableUnit:
		return d, command("systemctl", "enable", "--now", layout.UnitName)
	case RestartUnit:
		return d, command("systemctl", "restart", layout.UnitName)
	case StopUnit:
		return d, command("systemctl", "disable", "--now", layout.UnitName)
	case StopSlice:
		return d, command("systemctl", "stop", layout.IdleSlice)
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

// waitForDocker waits until Docker answers and, when oci is set, lists that
// runtime.
func waitForDocker(ctx context.Context, oci string) (*docker.Client, error) {
	for range 60 {
		if c, err := docker.Dial(ctx, layout.DockerSocket); err == nil {
			info, err := c.Info(ctx)
			if _, ok := info.Runtimes[oci]; err == nil && (oci == "" || ok) {
				return c, nil
			}
		}
		time.Sleep(time.Second)
	}
	if oci != "" {
		return nil, fmt.Errorf("docker did not list the %s runtime within a minute", oci)
	}
	return nil, errors.New("docker did not answer within a minute")
}

const daemonJSON = "/etc/docker/daemon.json"

// defaultPools are Docker's built-in IPv4 address pools
// (daemon/libnetwork/ipamutils), which /info leaves out when daemon.json sets
// none. Pinned without an IPv6 pool, Docker still derives its IPv6 one.
var defaultPools = []docker.AddressPool{
	{Base: "172.17.0.0/16", Size: 16}, {Base: "172.18.0.0/16", Size: 16}, {Base: "172.19.0.0/16", Size: 16},
	{Base: "172.20.0.0/14", Size: 16}, {Base: "172.24.0.0/14", Size: 16}, {Base: "172.28.0.0/14", Size: 16},
	{Base: "192.168.0.0/16", Size: 20},
}

// pinNetworks writes Docker's current bridge address and address pools into
// daemon.json. Sysbox's package otherwise sets its own and restarts Docker,
// and it refuses to install at all while any container exists.
func pinNetworks(ctx context.Context, d *docker.Client) error {
	info, err := d.Info(ctx)
	if err != nil {
		return err
	}
	bridge, ok, err := d.Network(ctx, "bridge")
	if err != nil {
		return err
	}
	bip := ""
	if ok && len(bridge.IPAM.Config) > 0 {
		bip = bridgeIP(bridge.IPAM.Config[0].Gateway, bridge.IPAM.Config[0].Subnet)
	}
	current, err := os.ReadFile(daemonJSON)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	pools := info.DefaultAddressPools
	if len(pools) == 0 {
		pools = defaultPools
	}
	pinned, err := withNetworks(current, bip, pools)
	if err != nil || bytes.Equal(pinned, current) {
		return err
	}
	return writeFile(WriteFile{Path: daemonJSON, Mode: 0o644, Content: pinned})
}

// bridgeIP is Docker's "bip" setting for a bridge: its gateway with the
// subnet's prefix length, or "" when either is unknown.
func bridgeIP(gateway, subnet string) string {
	_, prefix, ok := strings.Cut(subnet, "/")
	if gateway == "" || !ok {
		return ""
	}
	return gateway + "/" + prefix
}

// withNetworks adds "bip" and "default-address-pools" to a daemon.json when
// they are missing and known, and writes it indented as Sysbox's installer
// expects to find them. Every other setting keeps its value.
func withNetworks(current []byte, bip string, pools []docker.AddressPool) ([]byte, error) {
	settings := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(current)) > 0 {
		if err := json.Unmarshal(current, &settings); err != nil {
			return nil, fmt.Errorf("%s: %w", daemonJSON, err)
		}
	}
	if _, ok := settings["bip"]; !ok && bip != "" {
		settings["bip"], _ = json.Marshal(bip)
	}
	if _, ok := settings["default-address-pools"]; !ok && len(pools) > 0 {
		type pool struct {
			Base string `json:"base"`
			Size int    `json:"size"`
		}
		var list []pool
		for _, p := range pools {
			list = append(list, pool{p.Base, p.Size})
		}
		settings["default-address-pools"], _ = json.Marshal(list)
	}
	out, err := json.MarshalIndent(settings, "", "    ")
	return append(out, '\n'), err
}

// download fetches a Sysbox package into a new temporary directory and checks
// its SHA-256.
func download(ctx context.Context, p SysboxPackage) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s answered %s", p.URL, resp.Status)
	}
	dir, err := os.MkdirTemp("", "boxrunner-sysbox-")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, filepath.Base(p.URL))
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	sum := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, sum), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if got := hex.EncodeToString(sum.Sum(nil)); err == nil && got != p.SHA256 {
		err = fmt.Errorf("%s: SHA-256 is %s, want %s", p.URL, got, p.SHA256)
	}
	if err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return path, nil
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

// Package doctor checks that a box can run jobs safely, from the outside and
// from inside a throwaway job container.
package doctor

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/config"
	"github.com/dominikzabcik/boxrunner/internal/disk"
	"github.com/dominikzabcik/boxrunner/internal/docker"
	"github.com/dominikzabcik/boxrunner/internal/firewall"
	"github.com/dominikzabcik/boxrunner/internal/layout"
	"github.com/dominikzabcik/boxrunner/internal/mirror"
	"github.com/dominikzabcik/boxrunner/internal/release"
	"github.com/dominikzabcik/boxrunner/internal/status"
	"github.com/dominikzabcik/boxrunner/internal/worker"
)

type Result struct {
	Name   string
	OK     bool
	Detail string
}

type Checks struct {
	Config config.Config
	// ConfigErr is why the config could not be read; the checks that need it are skipped.
	ConfigErr error
	Worker    *worker.Client
	Exec      firewall.Run
	HTTP      *http.Client
	// Version is this agent's, and ReleasesURL boxrunner's latest-release page.
	Version     string
	ReleasesURL string
}

// MaxSkew is how far the clock may be off; GitHub's tokens carry times.
const MaxSkew = 30 * time.Second

// All runs every check and returns the results in order.
func (c Checks) All(ctx context.Context) []Result {
	var results []Result
	add := func(name string, ok bool, format string, args ...any) {
		results = append(results, Result{name, ok, fmt.Sprintf(format, args...)})
	}
	if c.ConfigErr != nil {
		add("config", false, "%v", c.ConfigErr)
	} else {
		add("config", true, "%s, box %s, %d classes", layout.ConfigPath, c.Config.Box, len(c.Config.Classes))
	}

	sysbox := false
	d, err := docker.Dial(ctx, layout.DockerSocket)
	if err != nil {
		add("docker", false, "%v", err)
	} else if info, err := d.Info(ctx); err != nil {
		add("docker", false, "%v", err)
	} else {
		add("docker", true, "Docker %s, API %s", info.ServerVersion, d.API)
		add("cgroup v2", info.CgroupVersion == "2", "cgroup version %s", info.CgroupVersion)
		add("cgroup driver", info.CgroupDriver == "systemd", "%s (boxrunner needs systemd)", info.CgroupDriver)
		_, sysbox = info.Runtimes[layout.SysboxRuntime]
		results = append(results, LiveRestore(info.LiveRestore, sysbox))
		if c.ConfigErr == nil {
			paths := []string{info.RootDir}
			if sysbox {
				paths = append(paths, layout.SysboxDir)
			}
			free, path, err := disk.Lowest(paths...)
			add("disk", err == nil && free >= int64(c.Config.DiskFloor), "%.1f GB free on %s, floor %.1f GB %s", float64(free)/1e9, path, float64(c.Config.DiskFloor)/1e9, orEmpty(err))
		}
	}

	withDocker := c.ConfigErr == nil && slices.ContainsFunc(c.Config.Classes, func(k config.Class) bool { return k.Docker })
	rules := firewall.Rules{Bridge: layout.Bridge}
	if withDocker && d != nil {
		rules.Mirror, _ = mirror.Find(ctx, d)
	}
	state := firewall.Check(ctx, c.Exec, rules)
	add("firewall rules", state == firewall.Current, "%s", map[firewall.State]string{
		firewall.Current: "in place for IPv4 and IPv6",
		firewall.Stale:   "incomplete; restart boxrunner or run install",
		firewall.Absent:  "missing; restart boxrunner or run install",
	}[state])

	if d != nil {
		results = append(results, c.probe(ctx, d)...)
		if c.ConfigErr == nil {
			results = append(results, isolation(ctx, d, c.Config.Classes, sysbox)...)
		}
		if withDocker {
			results = append(results, dockerInJobs(ctx, d, rules.Mirror)...)
		}
	}

	date, err := httpDate(ctx, c.HTTP, "https://github.com")
	if err != nil {
		add("clock", false, "could not read github.com's time: %v", err)
	} else {
		skew := time.Since(date)
		add("clock", skew.Abs() <= MaxSkew, "%s off github.com's clock", skew.Round(time.Second))
	}

	if c.ConfigErr == nil && c.Worker != nil {
		repos, err := c.Worker.Repos(ctx)
		if err != nil {
			add("worker", false, "%v", err)
		} else {
			add("worker", true, "%s accepts the token; %d repositories, classes %s", c.Config.Worker, len(repos.Repos), strings.Join(repos.Classes, ", "))
			for _, class := range c.Config.Classes {
				served := false
				for _, name := range repos.Classes {
					served = served || name == class.Name
				}
				if !served {
					add("class "+class.Name, false, "not one of the Worker's classes; jobs for it never arrive")
				}
			}
		}
	}
	// A stopped agent has no sessions or held classes to report; the other checks say what is wrong.
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if s, err := status.Fetch(sctx, layout.Socket); err == nil {
		results = append(results, Draining(s)...)
		results = append(results, Held(s, time.Now())...)
		results = append(results, Elsewhere(s)...)
	}
	return results
}

// LiveRestore says what restarting Docker does to running jobs. It never
// fails: install leaves live-restore off, because under Sysbox it breaks DNS
// in the jobs that survive a restart (docs/decisions.md).
func LiveRestore(enabled, sysbox bool) Result {
	switch {
	case enabled && sysbox:
		return Result{"live-restore", true, "on; jobs under Sysbox keep running through a Docker restart but lose DNS, so drain first (docs/upgrading.md#rebooting-the-box)"}
	case enabled:
		return Result{"live-restore", true, "on; running jobs keep running through a Docker restart"}
	default:
		return Result{"live-restore", true, "off; restarting Docker stops running jobs, so drain first (docs/upgrading.md#rebooting-the-box)"}
	}
}

// Draining fails while `boxrunner drain` keeps the agent from serving.
func Draining(s status.Snapshot) []Result {
	if !s.Draining {
		return nil
	}
	return []Result{{"draining", false, "the agent opens no sessions and starts no runners; resume with: sudo boxrunner resume"}}
}

// Elsewhere fails once for every repository and class the running agent
// cannot serve because another box's session holds the scale set.
func Elsewhere(s status.Snapshot) []Result {
	var results []Result
	for _, e := range s.Elsewhere {
		results = append(results, Result{"other box", false, e.Sentence()})
	}
	return results
}

// Held fails a check for each class that holds its starts because its
// runners fail before taking a job.
func Held(snap status.Snapshot, now time.Time) []Result {
	var results []Result
	for _, c := range snap.Classes {
		if next := c.NextTry(now); next != "" {
			results = append(results, Result{"class " + c.Name, false, "its runners exit before taking a job; " + next + "; see journalctl -u boxrunner"})
		}
	}
	return results
}

// Releases compares this agent and its Worker with boxrunner's latest release.
// Run it after All, which has the Worker answer. A result that is not OK is a
// warning: the box works, but a newer release is out or the check could not run.
func (c Checks) Releases(ctx context.Context) []Result {
	latest, err := release.Latest(ctx, c.HTTP, c.ReleasesURL)
	workerVersion, answered := "", false
	if c.Worker != nil {
		workerVersion, answered = c.Worker.WorkerVersion()
	}
	return Versions(c.Version, latest, err, workerVersion, answered)
}

// Versions words the results of Releases. The Worker gets a result only when
// it answered.
func Versions(agent, latest string, latestErr error, workerVersion string, workerAnswered bool) []Result {
	var results []Result
	switch {
	case latestErr != nil:
		results = append(results, Result{"release", false, fmt.Sprintf("could not find boxrunner's latest release: %v", latestErr)})
	case release.Newer(latest, agent):
		results = append(results, Result{"release", false, fmt.Sprintf("boxrunner %s is out and this agent is %s; upgrade with: sudo boxrunner upgrade", latest, agent)})
	default:
		results = append(results, Result{"release", true, fmt.Sprintf("agent %s, latest release %s", agent, latest)})
	}
	switch {
	case !workerAnswered:
	case workerVersion == "":
		results = append(results, Result{"worker release", false, "the Worker names no version, so it is older than the release that added it; redeploy it as docs/upgrading.md says"})
	case latestErr == nil && release.Newer(latest, workerVersion):
		results = append(results, Result{"worker release", false, fmt.Sprintf("the Worker runs %s and %s is out; redeploy it as docs/upgrading.md says", workerVersion, latest)})
	default:
		results = append(results, Result{"worker release", true, "Worker " + workerVersion})
	}
	return results
}

const probeScript = `probe() { timeout 3 bash -c "exec 3<>/dev/tcp/$2/$3" 2>/dev/null; echo "$1 $?"; }
probe host %s 22
%s
probe metadata 169.254.169.254 80
probe github github.com 443
`

// probe starts a container like a job's, in the idle slice on the boxrunner
// network, and tries to reach what jobs must not reach and what they must.
func (c Checks) probe(ctx context.Context, d *docker.Client) []Result {
	var results []Result
	add := func(name string, ok bool, format string, args ...any) {
		results = append(results, Result{name, ok, fmt.Sprintf(format, args...)})
	}
	network, ok, err := d.Network(ctx, layout.Network)
	if err != nil || !ok || len(network.IPAM.Config) == 0 {
		add("network", false, "the Docker network %s is missing; run install", layout.Network)
		return results
	}
	gateway := network.IPAM.Config[0].Gateway
	private := ""
	if gw, err := defaultGateway(); err == nil && gw.IsPrivate() {
		private = fmt.Sprintf("probe private %s 80", gw)
	}
	// A doctor that was interrupted leaves its container behind.
	d.Remove(ctx, "boxrunner-doctor")
	id, err := d.Create(ctx, docker.ContainerSpec{
		Name: "boxrunner-doctor", Image: layout.Image, Cmd: []string{"bash", "-c", fmt.Sprintf(probeScript, gateway, private)}, Tty: true,
		CgroupParent: layout.IdleSlice, Network: layout.Network, NanoCPUs: 1e9, Memory: 256 << 20, PidsLimit: 64,
	})
	if docker.NotFound(err) {
		add("probe container", false, "the job image %s is missing; run install", layout.Image)
		return results
	}
	if err != nil {
		add("probe container", false, "%v", err)
		return results
	}
	defer d.Remove(context.WithoutCancel(ctx), id)
	if err := d.Start(ctx, id); err != nil {
		add("probe container", false, "%v", err)
		return results
	}

	if pid, err := d.Pid(ctx, id); err != nil || pid == 0 {
		add("idle slice", false, "could not find the container's process: %v", err)
	} else {
		slice, idle, err := idleOf(pid)
		add("idle slice", err == nil && slice == "/"+layout.IdleSlice && idle == "1", "container runs in %s, cpu.idle %s %v", slice, idle, orEmpty(err))
	}

	if _, err := d.Wait(ctx, id); err != nil {
		add("probe container", false, "%v", err)
		return results
	}
	logs, _ := d.Logs(ctx, id)
	codes := ParseProbes(logs)
	// A dropped packet times out (124); a refusal or an unreachable answer
	// means something past the firewall saw it.
	blocked := func(name, what string) {
		code, ok := codes[name]
		switch {
		case !ok:
			add(name, false, "no answer from the probe for %s", what)
		case code == 0:
			add(name, false, "a job container reached %s", what)
		case code != 124:
			add(name, false, "a job container got an answer from %s (exit %d); the firewall should drop it", what, code)
		default:
			add(name, true, "%s is unreachable from jobs", what)
		}
	}
	blocked("host", "the host at "+gateway)
	if private != "" {
		blocked("private", "the private network's gateway")
	} else {
		add("private", true, "skipped: the default gateway is not a private address; the rules above cover private ranges")
	}
	blocked("metadata", "169.254.169.254")
	add("github", codes["github"] == 0, "github.com:443 from a job container (exit %d)", codes["github"])
	return results
}

// isolation starts a job container under sysbox-runc when a class runs its
// jobs there, reads which host uid the job's root is, and judges each class.
func isolation(ctx context.Context, d *docker.Client, classes []config.Class, sysbox bool) []Result {
	resolved := config.Config{Classes: classes}.ResolveIsolation(sysbox).Classes
	if !sysbox || !slices.ContainsFunc(resolved, func(k config.Class) bool { return k.Isolation == config.Sysbox }) {
		return Isolation(classes, sysbox, "", nil)
	}
	const name = "boxrunner-doctor-sysbox"
	d.Remove(ctx, name)
	id, err := d.Create(ctx, docker.ContainerSpec{
		Name: name, Image: layout.Image, Cmd: []string{"cat", "/proc/self/uid_map"}, Tty: true,
		Runtime: layout.SysboxRuntime, CgroupParent: layout.IdleSlice, Network: layout.Network, NanoCPUs: 1e9, Memory: 256 << 20, PidsLimit: 64,
	})
	if err == nil {
		defer d.Remove(context.WithoutCancel(ctx), id)
		err = d.Start(ctx, id)
	}
	if err == nil {
		_, err = d.Wait(ctx, id)
	}
	logs := ""
	if err == nil {
		logs, err = d.Logs(ctx, id)
	}
	return Isolation(classes, sysbox, logs, err)
}

// Isolation judges how each class keeps its jobs from the host. classes are
// as the config file sets them, sysbox says whether Docker lists sysbox-runc, and
// uidMap is /proc/self/uid_map as a job container under sysbox-runc read it,
// or why that container could not run.
func Isolation(classes []config.Class, sysbox bool, uidMap string, probeErr error) []Result {
	var results []Result
	resolved := config.Config{Classes: classes}.ResolveIsolation(sysbox).Classes
	for i, class := range resolved {
		r := Result{Name: "isolation " + class.Name}
		switch {
		case class.Isolation == config.Sysbox && !sysbox:
			r.Detail = "the class needs Sysbox, but Docker has no " + layout.SysboxRuntime + " runtime, so it gets no jobs; run sudo boxrunner install"
		case class.Isolation == config.Sysbox && probeErr != nil:
			r.Detail = fmt.Sprintf("sysbox: a job container could not start under %s: %v", layout.SysboxRuntime, probeErr)
		case class.Isolation == config.Sysbox:
			if uid, ok := rootOnHost(uidMap); ok && uid != 0 {
				r.OK, r.Detail = true, fmt.Sprintf("sysbox: a job's root is uid %d on the host", uid)
			} else {
				r.Detail = fmt.Sprintf("sysbox: a job's root is not mapped to an unprivileged user on the host (uid_map %q)", strings.TrimSpace(uidMap))
			}
		case classes[i].Isolation == config.Runc:
			r.OK, r.Detail = true, `runc, as the class asks: a job's root is root on the host`
		default:
			r.OK, r.Detail = true, "runc: a job's root is root on the host; sudo boxrunner install --sysbox maps it to an unprivileged user"
		}
		results = append(results, r)
	}
	return results
}

// rootOnHost reads a uid_map and returns the host uid that uid 0 maps to.
func rootOnHost(uidMap string) (int64, bool) {
	for _, line := range strings.Split(strings.ReplaceAll(uidMap, "\r", ""), "\n") {
		f := strings.Fields(line)
		if len(f) == 3 && f[0] == "0" {
			uid, err := strconv.ParseInt(f[1], 10, 64)
			return uid, err == nil
		}
	}
	return 0, false
}

// dockerScript runs as the runner user in a job container with Docker and
// reports what that Docker sees, one "name value" line each.
const dockerScript = `set -e
version=$(docker info --format '{{.ServerVersion}}')
mirrors=$(docker info --format '{{range .RegistryConfig.Mirrors}}{{.}} {{end}}')
answer=$(curl -s -o /dev/null --max-time 5 -w '%{http_code}' "$BOXRUNNER_MIRROR/v2/" || true)
echo "version $version"
echo "mirrors $mirrors"
echo "mirror $answer"
`

// dockerInJobs checks what classes with docker = true need: Docker's Sysbox
// runtime, a job container whose entrypoint starts a Docker daemon that the
// runner user can use, and the registry mirror at addr, which that daemon
// pulls through and reaches past the firewall.
func dockerInJobs(ctx context.Context, d *docker.Client, addr netip.AddrPort) []Result {
	var results []Result
	add := func(name string, ok bool, format string, args ...any) {
		results = append(results, Result{name, ok, fmt.Sprintf(format, args...)})
	}
	info, err := d.Info(ctx)
	if _, ok := info.Runtimes[layout.SysboxRuntime]; err != nil || !ok {
		add("sysbox", false, "Docker has no %s runtime, so classes with docker = true get no jobs; run sudo boxrunner install --docker", layout.SysboxRuntime)
		return results
	}
	add("sysbox", true, "Docker lists the %s runtime", layout.SysboxRuntime)

	env := []string{layout.EnvDocker}
	if addr.IsValid() {
		env = append(env, layout.EnvMirror+"="+mirror.URL(addr))
	}
	const name = "boxrunner-doctor-docker"
	d.Remove(ctx, name)
	id, err := d.Create(ctx, docker.ContainerSpec{
		Name: name, Image: layout.Image, Cmd: []string{"bash", "-c", dockerScript}, Tty: true, Env: env,
		Runtime: layout.SysboxRuntime, CgroupParent: layout.IdleSlice, Network: layout.Network, NanoCPUs: 1e9, Memory: 1 << 30, PidsLimit: 1024,
	})
	if err == nil {
		defer d.Remove(context.WithoutCancel(ctx), id)
		err = d.Start(ctx, id)
	}
	code := 0
	if err == nil {
		code, err = d.Wait(ctx, id)
	}
	if err != nil {
		add("docker in jobs", false, "%v", err)
		return results
	}
	logs, _ := d.Logs(ctx, id)
	return append(results, DockerInJobs(code, logs, addr)...)
}

// DockerInJobs judges what dockerScript reported from a job container that
// exited with code.
func DockerInJobs(code int, logs string, addr netip.AddrPort) []Result {
	logs = strings.TrimSpace(strings.ReplaceAll(logs, "\r", ""))
	if code != 0 {
		lines := strings.Split(logs, "\n")
		return []Result{{"docker in jobs", false, fmt.Sprintf("a job container could not start Docker (exit %d):\n%s", code, strings.Join(lines[max(0, len(lines)-10):], "\n"))}}
	}
	seen := map[string]string{}
	for _, line := range strings.Split(logs, "\n") {
		name, value, _ := strings.Cut(line, " ")
		seen[name] = strings.TrimSpace(value)
	}
	url := mirror.URL(addr)
	results := []Result{{"docker in jobs", true, fmt.Sprintf("a job container started Docker %s and the runner user can use it", seen["version"])}}
	switch {
	case !addr.IsValid():
		results = append(results, Result{"registry mirror", false, "the network " + layout.Network + " has no IPv4 gateway for the mirror; run install"})
	case !strings.Contains(seen["mirrors"]+" ", url+"/ "):
		results = append(results, Result{"registry mirror", false, fmt.Sprintf("a job's Docker does not pull through %s (its mirrors: %q); rebuild the job image with install", url, seen["mirrors"])})
	case seen["mirror"] != "200":
		results = append(results, Result{"registry mirror", false, fmt.Sprintf("%s did not answer a job container (HTTP %q); run install, or see docker logs %s", url, seen["mirror"], layout.Mirror)})
	default:
		results = append(results, Result{"registry mirror", true, fmt.Sprintf("a job's Docker pulls through %s, which answers from inside a job container", url)})
	}
	return results
}

// ParseProbes reads the probe script's "name exitcode" lines.
func ParseProbes(logs string) map[string]int {
	codes := map[string]int{}
	for _, line := range strings.Split(strings.ReplaceAll(logs, "\r", ""), "\n") {
		name, code, ok := strings.Cut(strings.TrimSpace(line), " ")
		if n, err := strconv.Atoi(code); ok && err == nil {
			codes[name] = n
		}
	}
	return codes
}

func orEmpty(err error) string {
	if err == nil {
		return ""
	}
	return "(" + err.Error() + ")"
}

// idleOf returns the slice a process's cgroup sits in and that slice's cpu.idle.
func idleOf(pid int) (string, string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return "", "", err
	}
	slice := path.Dir(CgroupPath(string(data)))
	idle, err := os.ReadFile(path.Join("/sys/fs/cgroup", slice, "cpu.idle"))
	return slice, strings.TrimSpace(string(idle)), err
}

// CgroupPath reads the cgroup v2 path from /proc/<pid>/cgroup.
func CgroupPath(procCgroup string) string {
	for _, line := range strings.Split(procCgroup, "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			return p
		}
	}
	return ""
}

func defaultGateway() (net.IP, error) {
	data, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return nil, err
	}
	return DefaultGateway(string(data))
}

// DefaultGateway reads the IPv4 default gateway from /proc/net/route.
func DefaultGateway(route string) (net.IP, error) {
	for _, line := range strings.Split(route, "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 3 || f[1] != "00000000" {
			continue
		}
		b, err := hex.DecodeString(f[2])
		if err != nil || len(b) != 4 {
			return nil, fmt.Errorf("unreadable gateway %q", f[2])
		}
		ip := make(net.IP, 4)
		binary.BigEndian.PutUint32(ip, binary.LittleEndian.Uint32(b))
		return ip, nil
	}
	return nil, fmt.Errorf("no default route")
}

func httpDate(ctx context.Context, client *http.Client, url string) (time.Time, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return time.Time{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return time.Time{}, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return http.ParseTime(resp.Header.Get("Date"))
}

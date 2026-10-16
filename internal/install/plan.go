// Package install converges a box to what boxrunner needs and takes it all
// away again. Plan and PlanUninstall are pure: observed state in, steps out.
// Execute runs the steps, and Install repeats observe, plan, execute until
// nothing is left to do.
package install

import (
	"bytes"
	"fmt"
	"io/fs"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/dominikzabcik/boxrunner/internal/config"
	"github.com/dominikzabcik/boxrunner/internal/firewall"
	"github.com/dominikzabcik/boxrunner/internal/layout"
	"github.com/dominikzabcik/boxrunner/internal/mirror"
)

// Desired is what the person running install asked for. Empty strings keep
// what the box already has.
type Desired struct {
	Worker   string
	Token    string
	Box      string
	Hostname string
	Host     config.Host
	// Binary is the running boxrunner, copied to /usr/local/bin.
	Binary []byte
	// Docker installs Sysbox so jobs can run Docker, and gives a new config's
	// classes docker = true.
	Docker bool
	// Sysbox installs Sysbox, so classes that leave isolation unset run under it.
	Sysbox bool
}

type File struct {
	Content []byte
	// Mode includes fs.ModeDir for a directory.
	Mode fs.FileMode
}

type Network struct {
	Exists bool
	// Problem says why an existing network named boxrunner is not ours.
	Problem string
	// Gateway is the host's IPv4 address on the bridge, where the registry
	// mirror listens.
	Gateway netip.Addr
}

type Observed struct {
	// Debian is true on Debian, Ubuntu and their derivatives, where install
	// can add Docker itself.
	Debian bool
	// Distro and DistroVersion are ID and VERSION_ID from /etc/os-release.
	Distro        string
	DistroVersion string
	// Kernel is the running kernel's release, such as 6.8.0-45-generic.
	Kernel string
	// Arch is Go's name for the CPU architecture, such as arm64.
	Arch string
	// Sysbox is true when Docker lists the sysbox-runc runtime.
	Sysbox       bool
	DockerBinary bool
	DockerUp     bool
	CgroupV2     bool
	CgroupDriver string
	// Paths holds every path boxrunner manages that exists.
	Paths   map[string]File
	Network Network
	// Firewall is the state of the rules with no service open to jobs, and
	// FirewallOpen with each set of services open at the network's gateway.
	Firewall     firewall.State
	FirewallOpen map[firewall.Services]firewall.State
	Mirror       mirror.State
	// Nft is true when the nft command is there, which classes with an egress
	// allowlist need.
	Nft bool
	// EgressNetworks are the Docker networks of egress classes, and EgressTable
	// is whether the nftables table of their rules exists.
	EgressNetworks []string
	EgressTable    bool
	Image          bool
	Containers     int
	UnitEnabled    bool
	UnitActive     bool
	// AgentReplaced means the running agent's binary was replaced after it
	// started, as install.sh does before it runs install.
	AgentReplaced bool
}

type Step interface{ String() string }

type (
	// Blocked is a precondition install cannot meet by itself.
	Blocked struct{ Reason string }
	// InstallDocker installs docker.io when Package is set, then enables and
	// starts the daemon.
	InstallDocker struct{ Package bool }
	// InstallSysbox installs the Sysbox package, which registers its runtime
	// with Docker.
	InstallSysbox struct{ Package SysboxPackage }
	// InstallNftables installs the nftables package, which holds the nft
	// command that the egress allowlists need.
	InstallNftables struct{}
	WriteFile       struct {
		Path    string
		Mode    fs.FileMode
		Content []byte
	}
	DaemonReload  struct{}
	CreateNetwork struct{}
	// ApplyFirewall installs the rules, with exceptions for the services
	// jobs may reach.
	ApplyFirewall struct{ Services firewall.Services }
	// StartMirror runs the registry mirror on the network's gateway.
	StartMirror      struct{}
	RemoveMirror     struct{}
	BuildImage       struct{}
	EnableUnit       struct{}
	RestartUnit      struct{}
	StopUnit         struct{}
	StopSlice        struct{}
	RemoveContainers struct{}
	RemoveNetwork    struct{}
	// RemoveEgress removes the networks of egress classes and the table of their rules.
	RemoveEgress   struct{ Networks []string }
	RemoveFirewall struct{}
	RemoveImage    struct{}
	RemovePath     struct{ Path string }
)

func (s Blocked) String() string { return "cannot continue: " + s.Reason }
func (s InstallDocker) String() string {
	if s.Package {
		return "install Docker (docker.io) and start it"
	}
	return "start Docker"
}
func (s InstallSysbox) String() string {
	return "install Sysbox " + s.Package.Version + "; Docker reloads its configuration and running containers keep running"
}
func (InstallNftables) String() string {
	return "install nftables, for the egress allowlists of classes"
}
func (s RemoveEgress) String() string {
	return "remove the egress allowlists' rules and networks"
}
func (s WriteFile) String() string  { return fmt.Sprintf("write %s (%04o)", s.Path, s.Mode) }
func (DaemonReload) String() string { return "reload systemd" }
func (CreateNetwork) String() string {
	return "create the Docker network " + layout.Network + " on bridge " + layout.Bridge
}
func (s ApplyFirewall) String() string {
	if s.Services != (firewall.Services{}) {
		return "apply the firewall rules for " + layout.Bridge + ", which let jobs reach " + s.Services.String()
	}
	return "apply the firewall rules for " + layout.Bridge
}
func (StartMirror) String() string {
	return fmt.Sprintf("start the registry mirror %s on %s's gateway, port %d", layout.Mirror, layout.Bridge, layout.MirrorPort)
}
func (RemoveMirror) String() string {
	return "remove the registry mirror " + layout.Mirror + " and its cache"
}
func (BuildImage) String() string       { return "build the job image " + layout.Image }
func (EnableUnit) String() string       { return "enable and start " + layout.UnitName }
func (RestartUnit) String() string      { return "restart " + layout.UnitName }
func (StopUnit) String() string         { return "stop and disable " + layout.UnitName + " (closes sessions)" }
func (StopSlice) String() string        { return "stop " + layout.IdleSlice }
func (RemoveContainers) String() string { return "remove the job containers" }
func (RemoveNetwork) String() string    { return "remove the Docker network " + layout.Network }
func (RemoveFirewall) String() string   { return "remove the firewall rules" }
func (RemoveImage) String() string      { return "remove the job image " + layout.Image }
func (s RemovePath) String() string     { return "remove " + s.Path }

// SysboxPackage is a pinned Sysbox release for one architecture.
type SysboxPackage struct {
	Version string
	URL     string
	SHA256  string
}

var sysboxPackages = map[string]SysboxPackage{
	"amd64": {"0.7.1", "https://github.com/nestybox/sysbox/releases/download/v0.7.1/sysbox-ce_0.7.1.linux_amd64.deb", "9d6d5484f980d0a17f86c492c1262015c2afb66280bdb97215b79fde6a0261c5"},
	"arm64": {"0.7.1", "https://github.com/nestybox/sysbox/releases/download/v0.7.1/sysbox-ce_0.7.1.linux_arm64.deb", "04ca894ae0b53f0fa54eaacc173ce40363c9a95ea5450f773716a84ef650a69b"},
}

// sysboxProblem says why Sysbox cannot be installed on the box, or "".
func sysboxProblem(o Observed) string {
	oldest := map[string]string{"ubuntu": "22.04", "debian": "11"}
	first, ok := oldest[o.Distro]
	switch {
	case !ok:
		return fmt.Sprintf("Sysbox supports Ubuntu 22.04 or newer and Debian 11 or newer; this box runs %q", o.Distro)
	case !atLeast(o.DistroVersion, first):
		return fmt.Sprintf("Sysbox supports %s %s or newer; this box runs %s", o.Distro, first, o.DistroVersion)
	case !atLeast(o.Kernel, "5.15"):
		return fmt.Sprintf("Sysbox needs Linux 5.15 or newer; this box runs %s", o.Kernel)
	}
	if _, ok := sysboxPackages[o.Arch]; !ok {
		return fmt.Sprintf("Sysbox has packages for amd64 and arm64; this box is %s", o.Arch)
	}
	return ""
}

var leadingVersion = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*`)

// atLeast compares the leading dotted numbers of two versions, so
// "6.8.0-45-generic" is at least "5.15" and "22.04" is not at least "24.04".
func atLeast(version, floor string) bool {
	have := strings.Split(leadingVersion.FindString(version), ".")
	for i, part := range strings.Split(floor, ".") {
		want, _ := strconv.Atoi(part)
		got := 0
		if i < len(have) {
			got, _ = strconv.Atoi(have[i])
		}
		if got != want {
			return got > want
		}
	}
	return true
}

const sliceUnit = `[Unit]
Description=boxrunner jobs that run only on cores nothing else wants

[Slice]
# systemd 252 and newer read the second line and set cpu.idle. Older ones
# ignore it and keep weight 1, which keeps the cpu controller on, and the
# agent sets cpu.idle itself.
CPUWeight=1
CPUWeight=idle
`

const serviceUnit = `[Unit]
Description=boxrunner agent: GitHub Actions jobs in fresh containers
After=docker.service network-online.target
Wants=network-online.target
Requires=docker.service
# Restarts by hand count too. 30 in ten minutes leaves room for maintenance,
# while a crash loop (one start every 5 s) still stops within three minutes.
StartLimitIntervalSec=10min
StartLimitBurst=30

[Service]
ExecStart=/usr/local/bin/boxrunner run
Restart=always
RestartSec=5
RestartPreventExitStatus=78
TimeoutStopSec=90

[Install]
WantedBy=multi-user.target
`

// Plan returns the steps that bring the box from o to d, or only Blocked
// steps when something must be fixed by hand first.
func Plan(d Desired, o Observed) []Step {
	var blocked []Step
	block := func(format string, args ...any) { blocked = append(blocked, Blocked{fmt.Sprintf(format, args...)}) }

	if !o.CgroupV2 {
		block("this box uses cgroup v1; boxrunner needs cgroup v2 (Ubuntu 22.04 and Debian 11 or newer use it by default)")
	}
	if !o.DockerBinary && !o.Debian {
		block("Docker is missing and this is not Debian or Ubuntu; install Docker Engine (https://docs.docker.com/engine/install/) and run install again")
	}
	if o.DockerUp && o.CgroupDriver != "systemd" {
		block(`Docker uses the %q cgroup driver; set "exec-opts": ["native.cgroupdriver=systemd"] in /etc/docker/daemon.json, restart Docker and run install again`, o.CgroupDriver)
	}
	if o.Network.Problem != "" {
		block("%s", o.Network.Problem)
	}

	content, cfg, cfgErr := desiredConfig(d, o)
	if cfgErr != nil {
		block("%v", cfgErr)
	}
	// A class with docker = true or isolation = "sysbox" needs Sysbox,
	// whether or not a flag asked. Jobs with Docker pull through the registry
	// mirror.
	wantMirror := slices.ContainsFunc(cfg.Classes, func(c config.Class) bool { return c.Docker })
	needSysbox := slices.ContainsFunc(cfg.Classes, func(c config.Class) bool { return c.Isolation == config.Sysbox })
	needNft := slices.ContainsFunc(cfg.Classes, func(c config.Class) bool { return c.Egress != nil }) && !o.Nft
	if needNft && !o.Debian {
		block("a class has an egress allowlist, which needs the nft command, and this is not Debian or Ubuntu; install nftables and run install again")
	}
	sysbox := !o.Sysbox && (d.Docker || d.Sysbox || needSysbox)
	if problem := sysboxProblem(o); sysbox && problem != "" {
		block("%s", problem)
	}
	if _, ok := o.Paths[layout.TokenPath]; !ok && d.Token == "" {
		block("no box token: pass --token or set BOXRUNNER_TOKEN")
	}
	if len(blocked) > 0 {
		return blocked
	}

	var steps []Step
	if !o.DockerBinary || !o.DockerUp {
		steps = append(steps, InstallDocker{Package: !o.DockerBinary})
	}
	if sysbox {
		steps = append(steps, InstallSysbox{sysboxPackages[o.Arch]})
	}
	if needNft {
		steps = append(steps, InstallNftables{})
	}
	files := []WriteFile{
		{layout.Binary, 0o755, d.Binary},
		{layout.IdleSlicePath, 0o644, []byte(sliceUnit)},
		{layout.UnitPath, 0o644, []byte(serviceUnit)},
		{layout.ConfigPath, 0o644, content},
	}
	if d.Token != "" {
		files = append(files, WriteFile{layout.TokenPath, 0o600, []byte(d.Token + "\n")})
	} else if f := o.Paths[layout.TokenPath]; f.Mode.Perm() != 0o600 {
		files = append(files, WriteFile{layout.TokenPath, 0o600, f.Content})
	}
	written := map[string]bool{}
	for _, f := range files {
		if have, ok := o.Paths[f.Path]; !ok || !bytes.Equal(have.Content, f.Content) || have.Mode.Perm() != f.Mode {
			steps = append(steps, f)
			written[f.Path] = true
		}
	}
	if written[layout.IdleSlicePath] || written[layout.UnitPath] {
		steps = append(steps, DaemonReload{})
	}
	if !o.Network.Exists {
		steps = append(steps, CreateNetwork{})
	}
	gateway := o.Network.Gateway
	switch {
	case wantMirror && (!gateway.IsValid() || o.Mirror != mirror.Current(mirror.Addr(gateway))):
		steps = append(steps, StartMirror{})
	case !wantMirror && o.Mirror != mirror.State{}:
		steps = append(steps, RemoveMirror{})
	}
	services := firewall.Services{Mirror: wantMirror, Cache: cfg.Cache > 0}
	wall := o.Firewall
	if services != (firewall.Services{}) {
		wall = o.FirewallOpen[services]
	}
	if wall != firewall.Current {
		steps = append(steps, ApplyFirewall{Services: services})
	}
	if !o.Image {
		steps = append(steps, BuildImage{})
	}
	switch {
	case !o.UnitEnabled || !o.UnitActive:
		steps = append(steps, EnableUnit{})
	case sysbox || o.AgentReplaced || written[layout.Binary] || written[layout.UnitPath] || written[layout.ConfigPath] || written[layout.TokenPath]:
		// The agent reads Docker's runtimes when it starts.
		steps = append(steps, RestartUnit{})
	}
	return steps
}

// desiredConfig is the config file install leaves: the existing one with
// worker and box filled in when given, or a new one sized for the host. It
// never overwrites anything else in an existing file.
func desiredConfig(d Desired, o Observed) ([]byte, config.Config, error) {
	var content string
	if f, ok := o.Paths[layout.ConfigPath]; ok {
		content = string(f.Content)
		if d.Worker != "" {
			content = config.SetTopLevel(content, "worker", d.Worker)
		}
		if d.Box != "" {
			content = config.SetTopLevel(content, "box", d.Box)
		}
	} else {
		if d.Worker == "" {
			return nil, config.Config{}, fmt.Errorf("no Worker URL: pass --worker or set BOXRUNNER_WORKER")
		}
		box := d.Box
		if box == "" {
			box = config.BoxName(d.Hostname)
		}
		content = config.Default(d.Worker, box, d.Host, d.Docker)
	}
	cfg, err := config.Parse([]byte(content), d.Hostname)
	if err != nil {
		return nil, config.Config{}, fmt.Errorf("%s: %w", layout.ConfigPath, err)
	}
	return []byte(content), cfg, nil
}

// PlanUninstall removes everything install created. Docker and Sysbox stay:
// other software on the box may use them.
func PlanUninstall(o Observed) []Step {
	var steps []Step
	if o.UnitEnabled || o.UnitActive {
		steps = append(steps, StopUnit{})
	}
	if o.DockerUp {
		if o.Containers > 0 {
			steps = append(steps, RemoveContainers{})
		}
		if o.Mirror != (mirror.State{}) {
			steps = append(steps, RemoveMirror{})
		}
		if len(o.EgressNetworks) > 0 {
			steps = append(steps, RemoveEgress{Networks: o.EgressNetworks})
		} else if o.EgressTable {
			steps = append(steps, RemoveEgress{})
		}
		if o.Network.Exists && o.Network.Problem == "" {
			steps = append(steps, RemoveNetwork{})
		}
		if o.Image {
			steps = append(steps, RemoveImage{})
		}
	}
	if !o.DockerUp && o.EgressTable {
		steps = append(steps, RemoveEgress{})
	}
	if o.Firewall != firewall.Absent {
		steps = append(steps, RemoveFirewall{})
	}
	if _, ok := o.Paths[layout.IdleSlicePath]; ok {
		// A slice stays loaded and active after its unit file is deleted.
		steps = append(steps, StopSlice{})
	}
	reload := false
	// An agent can exit before its status server removes the socket.
	for _, p := range []string{layout.UnitPath, layout.IdleSlicePath, layout.ConfigDir, layout.StateDir, layout.Socket, layout.Binary} {
		if _, ok := o.Paths[p]; ok {
			steps = append(steps, RemovePath{p})
			reload = reload || p == layout.UnitPath || p == layout.IdleSlicePath
		}
	}
	if reload {
		steps = append(steps, DaemonReload{})
	}
	return steps
}

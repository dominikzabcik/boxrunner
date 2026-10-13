// Package install converges a box to what boxrunner needs and takes it all
// away again. Plan and PlanUninstall are pure: observed state in, steps out.
// Execute runs the steps, and Install repeats observe, plan, execute until
// nothing is left to do.
package install

import (
	"bytes"
	"fmt"
	"io/fs"

	"github.com/dominikzabcik/boxrunner/internal/config"
	"github.com/dominikzabcik/boxrunner/internal/firewall"
	"github.com/dominikzabcik/boxrunner/internal/layout"
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
}

type Observed struct {
	// Debian is true on Debian, Ubuntu and their derivatives, where install
	// can add Docker itself.
	Debian       bool
	DockerBinary bool
	DockerUp     bool
	CgroupV2     bool
	CgroupDriver string
	// Paths holds every path boxrunner manages that exists.
	Paths       map[string]File
	Network     Network
	Firewall    firewall.State
	Image       bool
	Containers  int
	UnitEnabled bool
	UnitActive  bool
}

type Step interface{ String() string }

type (
	// Blocked is a precondition install cannot meet by itself.
	Blocked struct{ Reason string }
	// InstallDocker installs docker.io when Package is set, then enables and
	// starts the daemon.
	InstallDocker struct{ Package bool }
	WriteFile     struct {
		Path    string
		Mode    fs.FileMode
		Content []byte
	}
	DaemonReload     struct{}
	CreateNetwork    struct{}
	ApplyFirewall    struct{}
	BuildImage       struct{}
	EnableUnit       struct{}
	RestartUnit      struct{}
	StopUnit         struct{}
	RemoveContainers struct{}
	RemoveNetwork    struct{}
	RemoveFirewall   struct{}
	RemoveImage      struct{}
	RemovePath       struct{ Path string }
)

func (s Blocked) String() string { return "cannot continue: " + s.Reason }
func (s InstallDocker) String() string {
	if s.Package {
		return "install Docker (docker.io) and start it"
	}
	return "start Docker"
}
func (s WriteFile) String() string  { return fmt.Sprintf("write %s (%04o)", s.Path, s.Mode) }
func (DaemonReload) String() string { return "reload systemd" }
func (CreateNetwork) String() string {
	return "create the Docker network " + layout.Network + " on bridge " + layout.Bridge
}
func (ApplyFirewall) String() string    { return "apply the firewall rules for " + layout.Bridge }
func (BuildImage) String() string       { return "build the job image " + layout.Image }
func (EnableUnit) String() string       { return "enable and start " + layout.UnitName }
func (RestartUnit) String() string      { return "restart " + layout.UnitName }
func (StopUnit) String() string         { return "stop and disable " + layout.UnitName + " (closes sessions)" }
func (RemoveContainers) String() string { return "remove the job containers" }
func (RemoveNetwork) String() string    { return "remove the Docker network " + layout.Network }
func (RemoveFirewall) String() string   { return "remove the firewall rules" }
func (RemoveImage) String() string      { return "remove the job image " + layout.Image }
func (s RemovePath) String() string     { return "remove " + s.Path }

const sliceUnit = `[Unit]
Description=boxrunner jobs that run only on cores nothing else wants

[Slice]
CPUWeight=idle
`

const serviceUnit = `[Unit]
Description=boxrunner agent: GitHub Actions jobs in fresh containers
After=docker.service network-online.target
Wants=network-online.target
Requires=docker.service
StartLimitIntervalSec=10min
StartLimitBurst=5

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

	cfg, cfgErr := desiredConfig(d, o)
	if cfgErr != nil {
		block("%v", cfgErr)
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
	files := []WriteFile{
		{layout.Binary, 0o755, d.Binary},
		{layout.IdleSlicePath, 0o644, []byte(sliceUnit)},
		{layout.UnitPath, 0o644, []byte(serviceUnit)},
		{layout.ConfigPath, 0o644, cfg},
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
	if o.Firewall != firewall.Current {
		steps = append(steps, ApplyFirewall{})
	}
	if !o.Image {
		steps = append(steps, BuildImage{})
	}
	switch {
	case !o.UnitEnabled || !o.UnitActive:
		steps = append(steps, EnableUnit{})
	case written[layout.Binary] || written[layout.UnitPath] || written[layout.ConfigPath] || written[layout.TokenPath]:
		steps = append(steps, RestartUnit{})
	}
	return steps
}

// desiredConfig is the config file install leaves: the existing one with
// worker and box filled in when given, or a new one sized for the host. It
// never overwrites anything else in an existing file.
func desiredConfig(d Desired, o Observed) ([]byte, error) {
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
			return nil, fmt.Errorf("no Worker URL: pass --worker or set BOXRUNNER_WORKER")
		}
		box := d.Box
		if box == "" {
			box = config.BoxName(d.Hostname)
		}
		content = config.Default(d.Worker, box, d.Host)
	}
	if _, err := config.Parse([]byte(content), d.Hostname); err != nil {
		return nil, fmt.Errorf("%s: %w", layout.ConfigPath, err)
	}
	return []byte(content), nil
}

// PlanUninstall removes everything install created. Docker itself stays:
// other software on the box may use it.
func PlanUninstall(o Observed) []Step {
	var steps []Step
	if o.UnitEnabled || o.UnitActive {
		steps = append(steps, StopUnit{})
	}
	if o.DockerUp {
		if o.Containers > 0 {
			steps = append(steps, RemoveContainers{})
		}
		if o.Network.Exists && o.Network.Problem == "" {
			steps = append(steps, RemoveNetwork{})
		}
		if o.Image {
			steps = append(steps, RemoveImage{})
		}
	}
	if o.Firewall != firewall.Absent {
		steps = append(steps, RemoveFirewall{})
	}
	reload := false
	for _, p := range []string{layout.UnitPath, layout.IdleSlicePath, layout.ConfigDir, layout.StateDir, layout.Binary} {
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

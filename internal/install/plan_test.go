package install

import (
	"io/fs"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/dominikzabcik/boxrunner/internal/config"
	"github.com/dominikzabcik/boxrunner/internal/firewall"
	"github.com/dominikzabcik/boxrunner/internal/layout"
	"github.com/dominikzabcik/boxrunner/internal/mirror"
)

var desired = Desired{
	Worker:   "https://boxrunner.example.workers.dev",
	Token:    "box-token",
	Box:      "box-1",
	Hostname: "box-1.example.com",
	Host:     config.Host{CPUs: 4, Memory: 8 << 30},
	Binary:   []byte("boxrunner binary"),
}

func fresh() Observed {
	return Observed{
		Debian: true, Distro: "ubuntu", DistroVersion: "24.04", Kernel: "6.8.0-45-generic", Arch: "arm64",
		CgroupV2: true, Paths: map[string]File{},
	}
}

// installed is the box after a successful install with desired.
func installed() Observed {
	cfg := config.Default(desired.Worker, desired.Box, desired.Host, false)
	return Observed{
		Debian: true, Distro: "ubuntu", DistroVersion: "24.04", Kernel: "6.8.0-45-generic", Arch: "arm64",
		DockerBinary: true, DockerUp: true, CgroupV2: true, CgroupDriver: "systemd",
		Paths: map[string]File{
			layout.Binary:        {[]byte("boxrunner binary"), 0o755},
			layout.IdleSlicePath: {[]byte(sliceUnit), 0o644},
			layout.UnitPath:      {[]byte(serviceUnit), 0o644},
			layout.ConfigPath:    {[]byte(cfg), 0o644},
			layout.TokenPath:     {[]byte("box-token\n"), 0o600},
			layout.ConfigDir:     {nil, fs.ModeDir | 0o755},
			layout.StateDir:      {nil, fs.ModeDir | 0o755},
		},
		Network: Network{Exists: true, Gateway: gateway}, Firewall: firewall.Current, Image: true,
		UnitEnabled: true, UnitActive: true,
	}
}

var gateway = netip.MustParseAddr("172.18.0.1")

func names(steps []Step) []string {
	var out []string
	for _, s := range steps {
		out = append(out, s.String())
	}
	return out
}

func TestPlanFreshUbuntu(t *testing.T) {
	steps := Plan(desired, fresh())
	want := []string{
		"install Docker (docker.io) and start it",
		"write /usr/local/bin/boxrunner (0755)",
		"write /etc/systemd/system/boxrunner_idle.slice (0644)",
		"write /etc/systemd/system/boxrunner.service (0644)",
		"write /etc/boxrunner/config.toml (0644)",
		"write /etc/boxrunner/token (0600)",
		"reload systemd",
		"create the Docker network boxrunner on bridge boxrunner0",
		"apply the firewall rules for boxrunner0",
		"build the job image boxrunner/runner:local",
		"enable and start boxrunner.service",
	}
	if got := names(steps); !reflect.DeepEqual(got, want) {
		t.Fatalf("Plan =\n%q\nwant\n%q", got, want)
	}
	cfg, err := config.Parse(steps[4].(WriteFile).Content, "unused")
	if err != nil || cfg.Worker.String() != "https://boxrunner.example.workers.dev" || cfg.Box != "box-1" || len(cfg.Classes) != 2 {
		t.Errorf("new config = %+v, %v", cfg, err)
	}
	if tok := steps[5].(WriteFile); string(tok.Content) != "box-token\n" {
		t.Errorf("token file = %q", tok.Content)
	}
}

func TestPlanInstalledBoxDoesNothing(t *testing.T) {
	if got := Plan(desired, installed()); len(got) != 0 {
		t.Errorf("second install plans %q", names(got))
	}
	// Without flags, a second install keeps the token and config it finds.
	if got := Plan(Desired{Hostname: "box-1", Host: desired.Host, Binary: desired.Binary}, installed()); len(got) != 0 {
		t.Errorf("install without flags plans %q", names(got))
	}
}

func TestPlanUpgrade(t *testing.T) {
	d := desired
	d.Binary = []byte("newer boxrunner")
	o := installed()
	o.Firewall = firewall.Stale
	want := []string{"write /usr/local/bin/boxrunner (0755)", "apply the firewall rules for boxrunner0", "restart boxrunner.service"}
	if got := names(Plan(d, o)); !reflect.DeepEqual(got, want) {
		t.Errorf("Plan = %q", got)
	}
}

func TestPlanRestartsAnAgentWhoseBinaryInstallShReplaced(t *testing.T) {
	o := installed()
	o.AgentReplaced = true
	want := []string{"restart boxrunner.service"}
	if got := names(Plan(desired, o)); !reflect.DeepEqual(got, want) {
		t.Errorf("Plan = %q", got)
	}
}

func TestPlanKeepsAnExistingConfig(t *testing.T) {
	o := installed()
	existing := "# mine\nworker = \"https://old.example\"\n\n[[class]]\nname = \"big\"\ncpus = 16\nmemory = \"60GiB\"\nmax = 3\n"
	o.Paths[layout.ConfigPath] = File{[]byte(existing), 0o644}
	o.Paths[layout.TokenPath] = File{[]byte("box-token\n"), 0o644}
	d := desired
	d.Box, d.Token = "", ""
	steps := Plan(d, o)
	want := []string{"write /etc/boxrunner/config.toml (0644)", "write /etc/boxrunner/token (0600)", "restart boxrunner.service"}
	if got := names(steps); !reflect.DeepEqual(got, want) {
		t.Fatalf("Plan = %q", got)
	}
	wantConfig := "# mine\nworker = \"https://boxrunner.example.workers.dev\"\n\n[[class]]\nname = \"big\"\ncpus = 16\nmemory = \"60GiB\"\nmax = 3\n"
	if got := string(steps[0].(WriteFile).Content); got != wantConfig {
		t.Errorf("config =\n%s", got)
	}
	if got := string(steps[1].(WriteFile).Content); got != "box-token\n" {
		t.Errorf("token kept as %q", got)
	}
}

func TestPlanDocker(t *testing.T) {
	withDocker := desired
	withDocker.Docker = true
	sysbox := "install Sysbox 0.7.1; Docker reloads its configuration and running containers keep running"
	startMirror := "start the registry mirror boxrunner-mirror on boxrunner0's gateway, port 5000"
	mirrorFirewall := "apply the firewall rules for boxrunner0, which let jobs reach the registry mirror"
	withDockerClass := func(o *Observed) {
		o.Sysbox = true
		o.Paths[layout.ConfigPath] = File{[]byte(config.Default(desired.Worker, desired.Box, desired.Host, true)), 0o644}
	}
	cases := []struct {
		name    string
		desired Desired
		observe func(*Observed)
		want    []string
	}{
		{
			name:    "fresh box",
			desired: withDocker,
			observe: func(o *Observed) { *o = fresh() },
			want: []string{
				"install Docker (docker.io) and start it",
				sysbox,
				"write /usr/local/bin/boxrunner (0755)",
				"write /etc/systemd/system/boxrunner_idle.slice (0644)",
				"write /etc/systemd/system/boxrunner.service (0644)",
				"write /etc/boxrunner/config.toml (0644)",
				"write /etc/boxrunner/token (0600)",
				"reload systemd",
				"create the Docker network boxrunner on bridge boxrunner0",
				startMirror,
				mirrorFirewall,
				"build the job image boxrunner/runner:local",
				"enable and start boxrunner.service",
			},
		},
		{
			name:    "installed box without Sysbox",
			desired: withDocker,
			observe: func(*Observed) {},
			want:    []string{sysbox, "restart boxrunner.service"},
		},
		{
			name:    "Sysbox already installed",
			desired: withDocker,
			observe: func(o *Observed) { o.Sysbox = true },
			want:    nil,
		},
		{
			name:    "a class with docker = true and no flag",
			desired: desired,
			observe: func(o *Observed) {
				o.Paths[layout.ConfigPath] = File{[]byte(config.Default(desired.Worker, desired.Box, desired.Host, true)), 0o644}
			},
			want: []string{sysbox, startMirror, mirrorFirewall, "restart boxrunner.service"},
		},
		{
			name:    "a docker class with its mirror in place",
			desired: desired,
			observe: func(o *Observed) {
				withDockerClass(o)
				o.Mirror, o.FirewallOpen = mirror.Current(netip.MustParseAddrPort("172.18.0.1:5000")), map[firewall.Services]firewall.State{{Mirror: true}: firewall.Current}
			},
			want: nil,
		},
		{
			name:    "a docker class whose mirror stopped",
			desired: desired,
			observe: func(o *Observed) {
				withDockerClass(o)
				o.Mirror, o.FirewallOpen = mirror.Current(netip.MustParseAddrPort("172.18.0.1:5000")), map[firewall.Services]firewall.State{{Mirror: true}: firewall.Current}
				o.Mirror.Running = false
			},
			want: []string{startMirror},
		},
		{
			name:    "a docker class and a network on a new gateway",
			desired: desired,
			observe: func(o *Observed) {
				withDockerClass(o)
				o.Mirror, o.FirewallOpen = mirror.Current(netip.MustParseAddrPort("172.19.0.1:5000")), map[firewall.Services]firewall.State{{Mirror: true}: firewall.Stale}
			},
			want: []string{startMirror, mirrorFirewall},
		},
		{
			name:    "a mirror left after the last docker class went",
			desired: desired,
			observe: func(o *Observed) {
				o.Mirror = mirror.State{Volume: true}
			},
			want: []string{"remove the registry mirror boxrunner-mirror and its cache"},
		},
		{
			name:    "no flag and no docker class",
			desired: desired,
			observe: func(*Observed) {},
			want:    nil,
		},
		{
			name:    "Ubuntu 20.04",
			desired: withDocker,
			observe: func(o *Observed) { o.DistroVersion = "20.04" },
			want:    []string{"cannot continue: Sysbox supports ubuntu 22.04 or newer; this box runs 20.04"},
		},
		{
			name:    "Debian 12 on an old kernel",
			desired: withDocker,
			observe: func(o *Observed) { o.Distro, o.DistroVersion, o.Kernel = "debian", "12", "5.10.0-28-amd64" },
			want:    []string{"cannot continue: Sysbox needs Linux 5.15 or newer; this box runs 5.10.0-28-amd64"},
		},
		{
			name:    "Fedora",
			desired: withDocker,
			observe: func(o *Observed) { o.Distro, o.DistroVersion = "fedora", "41" },
			want:    []string{`cannot continue: Sysbox supports Ubuntu 22.04 or newer and Debian 11 or newer; this box runs "fedora"`},
		},
		{
			name:    "an architecture without a package",
			desired: withDocker,
			observe: func(o *Observed) { o.Arch = "riscv64" },
			want:    []string{"cannot continue: Sysbox has packages for amd64 and arm64; this box is riscv64"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := installed()
			c.observe(&o)
			steps := Plan(c.desired, o)
			if got := names(steps); !reflect.DeepEqual(got, c.want) {
				t.Errorf("Plan =\n%q\nwant\n%q", got, c.want)
			}
		})
	}
}

func TestPlanSysbox(t *testing.T) {
	withSysbox := desired
	withSysbox.Sysbox = true
	sysbox := "install Sysbox 0.7.1; Docker reloads its configuration and running containers keep running"
	sysboxClass := func(o *Observed) {
		o.Paths[layout.ConfigPath] = File{[]byte(string(o.Paths[layout.ConfigPath].Content) + "isolation = \"sysbox\"\n"), 0o644}
	}
	cases := []struct {
		name    string
		desired Desired
		observe func(*Observed)
		want    []string
	}{
		{
			name:    "fresh box",
			desired: withSysbox,
			observe: func(o *Observed) { *o = fresh() },
			want: []string{
				"install Docker (docker.io) and start it",
				sysbox,
				"write /usr/local/bin/boxrunner (0755)",
				"write /etc/systemd/system/boxrunner_idle.slice (0644)",
				"write /etc/systemd/system/boxrunner.service (0644)",
				"write /etc/boxrunner/config.toml (0644)",
				"write /etc/boxrunner/token (0600)",
				"reload systemd",
				"create the Docker network boxrunner on bridge boxrunner0",
				"apply the firewall rules for boxrunner0",
				"build the job image boxrunner/runner:local",
				"enable and start boxrunner.service",
			},
		},
		{
			name:    "installed box without Sysbox",
			desired: withSysbox,
			observe: func(*Observed) {},
			want:    []string{sysbox, "restart boxrunner.service"},
		},
		{
			name:    "Sysbox already installed",
			desired: withSysbox,
			observe: func(o *Observed) { o.Sysbox = true },
			want:    nil,
		},
		{
			name:    "a class with isolation = sysbox and no flag",
			desired: desired,
			observe: sysboxClass,
			want:    []string{sysbox, "restart boxrunner.service"},
		},
		{
			name:    "a class with isolation = sysbox on Fedora",
			desired: desired,
			observe: func(o *Observed) {
				sysboxClass(o)
				o.Distro, o.DistroVersion = "fedora", "41"
			},
			want: []string{`cannot continue: Sysbox supports Ubuntu 22.04 or newer and Debian 11 or newer; this box runs "fedora"`},
		},
		{
			name:    "no flag on a box Sysbox does not support",
			desired: desired,
			observe: func(o *Observed) { o.Distro, o.DistroVersion = "fedora", "41" },
			want:    nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := installed()
			c.observe(&o)
			steps := Plan(c.desired, o)
			if got := names(steps); !reflect.DeepEqual(got, c.want) {
				t.Errorf("Plan =\n%q\nwant\n%q", got, c.want)
			}
		})
	}

	// --sysbox leaves Docker inside jobs off in a new config.
	steps := Plan(withSysbox, fresh())
	cfg, err := config.Parse(steps[5].(WriteFile).Content, "unused")
	if err != nil || len(cfg.Classes) != 2 || cfg.Classes[0].Docker || cfg.Classes[1].Docker || cfg.Classes[0].Isolation != config.AutoIsolation {
		t.Errorf("new config = %+v, %v", cfg.Classes, err)
	}
}

func TestPlanDockerWritesDockerClasses(t *testing.T) {
	d := desired
	d.Docker = true
	steps := Plan(d, fresh())
	if got := steps[1].(InstallSysbox).Package.URL; got != "https://github.com/nestybox/sysbox/releases/download/v0.7.1/sysbox-ce_0.7.1.linux_arm64.deb" {
		t.Errorf("package = %s", got)
	}
	cfg, err := config.Parse(steps[5].(WriteFile).Content, "unused")
	if err != nil || len(cfg.Classes) != 2 || !cfg.Classes[0].Docker || !cfg.Classes[1].Docker {
		t.Errorf("new config = %+v, %v", cfg.Classes, err)
	}
}

func TestPlanBlocked(t *testing.T) {
	cases := []struct {
		name    string
		desired Desired
		observe func(*Observed)
		want    []string
	}{
		{
			name: "cgroup v1 and no Docker outside Debian",
			observe: func(o *Observed) {
				o.CgroupV2, o.Debian = false, false
			},
			desired: desired,
			want: []string{
				"cannot continue: this box uses cgroup v1; boxrunner needs cgroup v2 (Ubuntu 22.04 and Debian 11 or newer use it by default)",
				"cannot continue: Docker is missing and this is not Debian or Ubuntu; install Docker Engine (https://docs.docker.com/engine/install/) and run install again",
			},
		},
		{
			name:    "cgroupfs driver and a foreign network",
			desired: desired,
			observe: func(o *Observed) {
				o.DockerBinary, o.DockerUp, o.CgroupDriver = true, true, "cgroupfs"
				o.Network = Network{Exists: true, Problem: "a network named boxrunner exists with bridge docker1; remove it first"}
			},
			want: []string{
				`cannot continue: Docker uses the "cgroupfs" cgroup driver; set "exec-opts": ["native.cgroupdriver=systemd"] in /etc/docker/daemon.json, restart Docker and run install again`,
				"cannot continue: a network named boxrunner exists with bridge docker1; remove it first",
			},
		},
		{
			name:    "nothing to install with",
			desired: Desired{Hostname: "box-1", Host: desired.Host},
			observe: func(*Observed) {},
			want: []string{
				"cannot continue: no Worker URL: pass --worker or set BOXRUNNER_WORKER",
				"cannot continue: no box token: pass --token or set BOXRUNNER_TOKEN",
			},
		},
		{
			name:    "a worker URL that would break the config",
			desired: func() Desired { d := desired; d.Worker = "http://boxrunner.example.com"; return d }(),
			observe: func(*Observed) {},
			want: []string{
				"cannot continue: /etc/boxrunner/config.toml: config:\nworker: \"http://boxrunner.example.com\" must use https",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := fresh()
			c.observe(&o)
			if got := names(Plan(c.desired, o)); !reflect.DeepEqual(got, c.want) {
				t.Errorf("Plan =\n%q\nwant\n%q", got, c.want)
			}
		})
	}
}

func TestPlanUninstall(t *testing.T) {
	o := installed()
	o.Containers = 2
	o.Mirror = mirror.State{Identity: mirror.Identity(netip.MustParseAddrPort("172.18.0.1:5000")), Volume: true}
	o.Paths["/run/boxrunner.sock"] = File{Mode: fs.ModeSocket | 0o600}
	want := []string{
		"stop and disable boxrunner.service (closes sessions)",
		"remove the job containers",
		"remove the registry mirror boxrunner-mirror and its cache",
		"remove the Docker network boxrunner",
		"remove the job image boxrunner/runner:local",
		"remove the firewall rules",
		"stop boxrunner_idle.slice",
		"remove /etc/systemd/system/boxrunner.service",
		"remove /etc/systemd/system/boxrunner_idle.slice",
		"remove /etc/boxrunner",
		"remove /var/lib/boxrunner",
		"remove /run/boxrunner.sock",
		"remove /usr/local/bin/boxrunner",
		"reload systemd",
	}
	if got := names(PlanUninstall(o)); !reflect.DeepEqual(got, want) {
		t.Errorf("PlanUninstall =\n%q\nwant\n%q", got, want)
	}
	clean := Observed{DockerBinary: true, DockerUp: true, CgroupV2: true, Paths: map[string]File{}}
	if got := PlanUninstall(clean); len(got) != 0 {
		t.Errorf("uninstall on a clean box plans %q", names(got))
	}
}

func TestPlanCache(t *testing.T) {
	withCache := func(o *Observed, docker bool) {
		cfg := config.Default(desired.Worker, desired.Box, desired.Host, docker)
		cfg = strings.Replace(cfg, "[[class]]\nname = \"boxrunner\"\n", "cache = \"20GB\"\n\n[[class]]\nname = \"boxrunner\"\ncache = true\n", 1)
		o.Paths[layout.ConfigPath] = File{[]byte(cfg), 0o644}
	}
	cases := []struct {
		name    string
		observe func(*Observed)
		want    []string
	}{
		{
			name:    "the cache turned on",
			observe: func(o *Observed) { withCache(o, false) },
			want:    []string{"apply the firewall rules for boxrunner0, which let jobs reach the Actions cache"},
		},
		{
			name: "the cache with its rules in place",
			observe: func(o *Observed) {
				withCache(o, false)
				o.FirewallOpen = map[firewall.Services]firewall.State{{Cache: true}: firewall.Current}
			},
			want: nil,
		},
		{
			name: "the cache and a docker class",
			observe: func(o *Observed) {
				withCache(o, true)
				o.Sysbox = true
				o.Mirror = mirror.Current(netip.MustParseAddrPort("172.18.0.1:5000"))
				o.FirewallOpen = map[firewall.Services]firewall.State{{Mirror: true}: firewall.Current, {Cache: true}: firewall.Stale}
			},
			want: []string{"apply the firewall rules for boxrunner0, which let jobs reach the registry mirror and the Actions cache"},
		},
		{
			name: "the cache turned off",
			observe: func(o *Observed) {
				o.Firewall = firewall.Stale
				o.FirewallOpen = map[firewall.Services]firewall.State{{Cache: true}: firewall.Current}
			},
			want: []string{"apply the firewall rules for boxrunner0"},
		},
	}
	for _, c := range cases {
		o := installed()
		c.observe(&o)
		if got := names(Plan(desired, o)); !slices.Equal(got, c.want) {
			t.Errorf("%s: Plan =\n%s\nwant\n%s", c.name, strings.Join(got, "\n"), strings.Join(c.want, "\n"))
		}
	}
}

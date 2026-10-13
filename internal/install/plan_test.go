package install

import (
	"io/fs"
	"reflect"
	"testing"

	"github.com/dominikzabcik/boxrunner/internal/config"
	"github.com/dominikzabcik/boxrunner/internal/firewall"
	"github.com/dominikzabcik/boxrunner/internal/layout"
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
	return Observed{Debian: true, CgroupV2: true, Paths: map[string]File{}}
}

// installed is the box after a successful install with desired.
func installed() Observed {
	cfg := config.Default(desired.Worker, desired.Box, desired.Host)
	return Observed{
		Debian: true, DockerBinary: true, DockerUp: true, CgroupV2: true, CgroupDriver: "systemd",
		Paths: map[string]File{
			layout.Binary:        {[]byte("boxrunner binary"), 0o755},
			layout.IdleSlicePath: {[]byte(sliceUnit), 0o644},
			layout.UnitPath:      {[]byte(serviceUnit), 0o644},
			layout.ConfigPath:    {[]byte(cfg), 0o644},
			layout.TokenPath:     {[]byte("box-token\n"), 0o600},
			layout.ConfigDir:     {nil, fs.ModeDir | 0o755},
			layout.StateDir:      {nil, fs.ModeDir | 0o755},
		},
		Network: Network{Exists: true}, Firewall: firewall.Current, Image: true,
		UnitEnabled: true, UnitActive: true,
	}
}

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
	want := []string{
		"stop and disable boxrunner.service (closes sessions)",
		"remove the job containers",
		"remove the Docker network boxrunner",
		"remove the job image boxrunner/runner:local",
		"remove the firewall rules",
		"remove /etc/systemd/system/boxrunner.service",
		"remove /etc/systemd/system/boxrunner_idle.slice",
		"remove /etc/boxrunner",
		"remove /var/lib/boxrunner",
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

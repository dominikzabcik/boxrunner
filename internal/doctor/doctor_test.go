package doctor

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/config"
	"github.com/dominikzabcik/boxrunner/internal/status"
	"github.com/dominikzabcik/boxrunner/internal/worker"
)

func TestParseProbes(t *testing.T) {
	logs := "host 124\r\nprivate 124\r\nmetadata 1\r\ngithub 0\r\nbash: warning\r\n"
	want := map[string]int{"host": 124, "private": 124, "metadata": 1, "github": 0}
	if got := ParseProbes(logs); !reflect.DeepEqual(got, want) {
		t.Errorf("ParseProbes = %v", got)
	}
}

func TestDockerInJobs(t *testing.T) {
	addr := netip.MustParseAddrPort("172.18.0.1:5000")
	cases := []struct {
		name string
		code int
		logs string
		want []Result
	}{
		{
			name: "pulls through the mirror",
			logs: "version 29.8.2\r\nmirrors http://172.18.0.1:5000/ \r\nmirror 200\r\n",
			want: []Result{
				{"docker in jobs", true, "a job container started Docker 29.8.2 and the runner user can use it"},
				{"registry mirror", true, "a job's Docker pulls through http://172.18.0.1:5000, which answers from inside a job container"},
			},
		},
		{
			name: "an image built before the mirror",
			logs: "version 29.8.2\r\nmirrors \r\nmirror 200\r\n",
			want: []Result{
				{"docker in jobs", true, "a job container started Docker 29.8.2 and the runner user can use it"},
				{"registry mirror", false, `a job's Docker does not pull through http://172.18.0.1:5000 (its mirrors: ""); rebuild the job image with install`},
			},
		},
		{
			name: "the firewall drops the mirror",
			logs: "version 29.8.2\r\nmirrors http://172.18.0.1:5000/ \r\nmirror 000\r\n",
			want: []Result{
				{"docker in jobs", true, "a job container started Docker 29.8.2 and the runner user can use it"},
				{"registry mirror", false, `http://172.18.0.1:5000 did not answer a job container (HTTP "000"); run install, or see docker logs boxrunner-mirror`},
			},
		},
		{
			name: "no daemon",
			code: 1,
			logs: "boxrunner: the job's Docker daemon did not start:\r\nfailed to start daemon\r\n",
			want: []Result{{"docker in jobs", false, "a job container could not start Docker (exit 1):\nboxrunner: the job's Docker daemon did not start:\nfailed to start daemon"}},
		},
	}
	for _, c := range cases {
		if got := DockerInJobs(c.code, c.logs, addr); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: DockerInJobs =\n%+v\nwant\n%+v", c.name, got, c.want)
		}
	}
}

func TestIdleSlice(t *testing.T) {
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "boxrunner_idle.slice"), 0o755)
	cgroup := "/boxrunner_idle.slice/docker-3f9a.scope"
	cases := []struct {
		name    string
		cgroup  string
		cpuIdle string
		want    Result
	}{
		{"idle", cgroup, "1\n", Result{"idle slice", true, "container runs in /boxrunner_idle.slice, cpu.idle 1"}},
		{"not idle", cgroup, "0\n", Result{"idle slice", false, "container runs in /boxrunner_idle.slice, cpu.idle 0; restart boxrunner to set it"}},
		{"kernel without cpu.idle", cgroup, "", Result{"idle slice", false, "this kernel (5.10.0-35-amd64) has no cpu.idle, which Linux 5.15 added, so idle jobs do not run as SCHED_IDLE; install a newer kernel"}},
		{"another slice", "/system.slice/docker-3f9a.scope", "1\n", Result{"idle slice", false, "container runs in /system.slice, not /boxrunner_idle.slice"}},
	}
	for _, c := range cases {
		file := filepath.Join(root, "boxrunner_idle.slice", "cpu.idle")
		os.Remove(file)
		if c.cpuIdle != "" {
			os.WriteFile(file, []byte(c.cpuIdle), 0o644)
		}
		if got := IdleSlice(root, c.cgroup, "5.10.0-35-amd64"); got != c.want {
			t.Errorf("%s: IdleSlice = %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestCgroupPath(t *testing.T) {
	if got := CgroupPath("0::/boxrunner_idle.slice/docker-3f9a.scope\n"); got != "/boxrunner_idle.slice/docker-3f9a.scope" {
		t.Errorf("CgroupPath = %q", got)
	}
}

func TestDefaultGateway(t *testing.T) {
	route := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"eth0\t0001A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n" +
		"eth0\t00000000\t0101A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n"
	got, err := DefaultGateway(route)
	if err != nil || got.String() != "192.168.1.1" {
		t.Errorf("DefaultGateway = %v, %v", got, err)
	}
	if _, err := DefaultGateway("Iface\tDestination\n"); err == nil || err.Error() != "no default route" {
		t.Errorf("DefaultGateway without a default route = %v", err)
	}
}

func TestIsolation(t *testing.T) {
	classes := []config.Class{{Name: "boxrunner"}, {Name: "boxrunner-light", Isolation: config.Runc}, {Name: "docker", Docker: true, Isolation: config.Sysbox}}
	cases := []struct {
		name   string
		sysbox bool
		uidMap string
		err    error
		want   []Result
	}{
		{
			name:   "root maps to an unprivileged user",
			sysbox: true,
			uidMap: "         0     165536      65536\r\n",
			want: []Result{
				{"isolation boxrunner", true, "sysbox: a job's root is uid 165536 on the host"},
				{"isolation boxrunner-light", true, "runc, as the class asks: a job's root is root on the host"},
				{"isolation docker", true, "sysbox: a job's root is uid 165536 on the host"},
			},
		},
		{
			name:   "root maps to root",
			sysbox: true,
			uidMap: "0 0 4294967295\n",
			want: []Result{
				{"isolation boxrunner", false, `sysbox: a job's root is not mapped to an unprivileged user on the host (uid_map "0 0 4294967295")`},
				{"isolation boxrunner-light", true, "runc, as the class asks: a job's root is root on the host"},
				{"isolation docker", false, `sysbox: a job's root is not mapped to an unprivileged user on the host (uid_map "0 0 4294967295")`},
			},
		},
		{
			name:   "the probe container failed",
			sysbox: true,
			err:    errors.New("image not found"),
			want: []Result{
				{"isolation boxrunner", false, "sysbox: a job container could not start under sysbox-runc: image not found"},
				{"isolation boxrunner-light", true, "runc, as the class asks: a job's root is root on the host"},
				{"isolation docker", false, "sysbox: a job container could not start under sysbox-runc: image not found"},
			},
		},
		{
			name: "no Sysbox on the box",
			want: []Result{
				{"isolation boxrunner", true, "runc: a job's root is root on the host; sudo boxrunner install --sysbox maps it to an unprivileged user"},
				{"isolation boxrunner-light", true, "runc, as the class asks: a job's root is root on the host"},
				{"isolation docker", false, "the class needs Sysbox, but Docker has no sysbox-runc runtime, so it gets no jobs; run sudo boxrunner install"},
			},
		},
	}
	for _, c := range cases {
		if got := Isolation(classes, c.sysbox, c.uidMap, c.err); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: Isolation =\n%+v\nwant\n%+v", c.name, got, c.want)
		}
	}
}

func TestElsewhere(t *testing.T) {
	snap := status.Snapshot{
		ScaleSets: []status.ScaleSet{{Repo: "octo/site", Class: "boxrunner", Assigned: 1}},
		Elsewhere: []status.Elsewhere{{Repo: "octo/docs", Class: "boxrunner", Box: "box-2"}},
	}
	want := []Result{{"other box", false, "box-2 serves octo/docs boxrunner; this box takes its jobs only after box-2 stops. Give each box its own classes or repos (docs/config.md#several-boxes)."}}
	if got := Elsewhere(snap); !reflect.DeepEqual(got, want) {
		t.Errorf("Elsewhere =\n%+v\nwant\n%+v", got, want)
	}
}

func TestHeld(t *testing.T) {
	now := time.Date(2026, 10, 13, 21, 0, 0, 0, time.UTC)
	snap := status.Snapshot{Classes: []status.Class{{Name: "boxrunner"}, {Name: "web", HeldUntil: now.Add(-time.Second)}}}
	want := []Result{{"class web", false, "its runners exit before taking a job; trying one runner now; see journalctl -u boxrunner"}}
	if got := Held(snap, now); !reflect.DeepEqual(got, want) {
		t.Errorf("Held = %+v, want %+v", got, want)
	}
}

func TestRunnerImage(t *testing.T) {
	refused := "ghcr.io/actions/actions-runner:2.339.0 points to sha256:0bad, not to sha256:e549 that boxrunner's CI verified"
	verified := Result{"runner image", true, "sha256:4ffadc0002b2581327e06101fc8c06cd189232baf79fe561fac9caeb76f5e807, which boxrunner's CI verified"}
	cases := []struct {
		name string
		img  status.ImageStatus
		want []Result
	}{
		{"no job image yet", status.ImageStatus{}, nil},
		{"verified", status.ImageStatus{Built: "2.338.0+3f9a2c0b1d4e", Digest: "sha256:4ffadc0002b2581327e06101fc8c06cd189232baf79fe561fac9caeb76f5e807"}, []Result{verified}},
		{
			"built before the list", status.ImageStatus{Built: "2.338.0+5be01c7d2a9e"},
			[]Result{{"runner image", false, "the job image was built before boxrunner built from the runner images its CI verified; the agent rebuilds it while no normal-priority job runs"}},
		},
		{
			"the first job image building", status.ImageStatus{Building: "2.338.0+3f9a2c0b1d4e"},
			[]Result{{"runner image", true, "the agent is building job image 2.338.0+3f9a2c0b1d4e or its class images"}},
		},
		{
			"a newer image building", status.ImageStatus{Built: "2.338.0+3f9a2c0b1d4e", Digest: "sha256:4ffadc0002b2581327e06101fc8c06cd189232baf79fe561fac9caeb76f5e807", Building: "2.339.0+3f9a2c0b1d4e"},
			[]Result{verified, {"runner image", true, "the agent is building job image 2.339.0+3f9a2c0b1d4e or its class images"}},
		},
		{
			"a newer image refused", status.ImageStatus{Built: "2.338.0+3f9a2c0b1d4e", Digest: "sha256:4ffadc0002b2581327e06101fc8c06cd189232baf79fe561fac9caeb76f5e807", Refused: refused},
			[]Result{verified, {"runner image", false, "refused to build: " + refused + "; jobs keep the current image, and the agent tries again in an hour; see journalctl -u boxrunner"}},
		},
	}
	for _, c := range cases {
		if got := RunnerImage(c.img); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: RunnerImage = %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestUnlistedRunner(t *testing.T) {
	if got := UnlistedRunner(status.ImageStatus{Built: "2.338.0+3f9a2c0b1d4e", Latest: "2.338.0+3f9a2c0b1d4e"}); got != nil {
		t.Errorf("UnlistedRunner with the latest release listed = %+v", got)
	}
	got := UnlistedRunner(status.ImageStatus{Built: "2.338.0+3f9a2c0b1d4e", Latest: "2.338.0+3f9a2c0b1d4e", Unlisted: "2.339.0"})
	want := []Result{{"runner release", false, "runner 2.339.0 is out, but boxrunner's CI has not verified its image yet; the agent builds 2.338.0 until it does, and checks the list hourly"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("UnlistedRunner = %+v, want %+v", got, want)
	}
}

func TestVersions(t *testing.T) {
	upgrade := Result{"release", false, "boxrunner 0.2.2 is out and this agent is 0.2.1; upgrade with: sudo boxrunner upgrade"}
	cases := []struct {
		name           string
		agent, latest  string
		latestErr      error
		worker         string
		workerAnswered bool
		want           []Result
	}{
		{
			name: "both current", agent: "0.2.2", latest: "0.2.2", worker: "0.2.2", workerAnswered: true,
			want: []Result{{"release", true, "agent 0.2.2, latest release 0.2.2"}, {"worker release", true, "Worker 0.2.2"}},
		},
		{
			name: "both behind", agent: "0.2.1", latest: "0.2.2", worker: "0.2.1", workerAnswered: true,
			want: []Result{upgrade, {"worker release", false, "the Worker runs 0.2.1 and 0.2.2 is out; redeploy it as docs/upgrading.md says"}},
		},
		{
			name: "a Worker that names no version", agent: "0.2.1", latest: "0.2.2", worker: "", workerAnswered: true,
			want: []Result{upgrade, {"worker release", false, "the Worker names no version, so it is older than the release that added it; redeploy it as docs/upgrading.md says"}},
		},
		{
			name: "a Worker that did not answer", agent: "0.2.1", latest: "0.2.2",
			want: []Result{upgrade},
		},
		{
			name: "github.com out of reach", agent: "0.2.2", latestErr: errors.New("latest release: timeout"), worker: "0.2.2", workerAnswered: true,
			want: []Result{{"release", false, "could not find boxrunner's latest release: latest release: timeout"}, {"worker release", true, "Worker 0.2.2"}},
		},
	}
	for _, c := range cases {
		if got := Versions(c.agent, c.latest, c.latestErr, c.worker, c.workerAnswered); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: Versions =\n%+v\nwant\n%+v", c.name, got, c.want)
		}
	}
}

func TestLiveRestore(t *testing.T) {
	cases := []struct {
		enabled, sysbox bool
		want            Result
	}{
		{false, true, Result{"live-restore", true, "off; restarting Docker stops running jobs, so drain first (docs/upgrading.md#rebooting-the-box)"}},
		{true, true, Result{"live-restore", true, "on; jobs under Sysbox keep running through a Docker restart but lose DNS, so drain first (docs/upgrading.md#rebooting-the-box)"}},
		{true, false, Result{"live-restore", true, "on; running jobs keep running through a Docker restart"}},
	}
	for _, c := range cases {
		if got := LiveRestore(c.enabled, c.sysbox); got != c.want {
			t.Errorf("LiveRestore(%v, %v) = %+v, want %+v", c.enabled, c.sysbox, got, c.want)
		}
	}
}

func TestDraining(t *testing.T) {
	if got := Draining(status.Snapshot{}); got != nil {
		t.Errorf("Draining of a serving agent = %+v", got)
	}
	want := []Result{{"draining", false, "the agent opens no sessions and starts no runners; resume with: sudo boxrunner resume"}}
	if got := Draining(status.Snapshot{Draining: true}); !reflect.DeepEqual(got, want) {
		t.Errorf("Draining = %+v, want %+v", got, want)
	}
}

func TestForks(t *testing.T) {
	settings := func(run, approval, write, secrets bool) *worker.ForkPullRequests {
		return &worker.ForkPullRequests{RunWorkflows: run, RequireApproval: approval, SendWriteTokens: write, SendSecrets: secrets}
	}
	all := []config.Class{{Name: "boxrunner"}}
	cases := []struct {
		name    string
		repos   []worker.Repo
		classes []config.Class
		want    []Result
	}{
		{
			name: "fork workflows off, or on with approval and a read token",
			repos: []worker.Repo{
				{Name: "octo/site", ForkPullRequests: settings(false, false, true, true)},
				{Name: "octo/docs", ForkPullRequests: settings(true, true, false, false)},
			},
			classes: all,
			want:    []Result{{"fork PRs", true, "no repository runs workflows from fork pull requests without approval, with write tokens or with secrets"}},
		},
		{
			name: "without approval, with write tokens and secrets",
			repos: []worker.Repo{
				{Name: "octo/site", ForkPullRequests: settings(true, false, true, true)},
				{Name: "octo/docs", ForkPullRequests: settings(true, true, false, true)},
				{Name: "octo/api"},
			},
			classes: all,
			want: []Result{
				{"fork PRs", false, `octo/site runs workflows from fork pull requests without approval, with write tokens and with secrets; in its settings, under Actions, General, Fork pull request workflows, turn on "Require approval for fork pull request workflows", turn off "Send write tokens to workflows from pull requests" and turn off "Send secrets to workflows from pull requests", or turn off "Run workflows from fork pull requests"`},
				{"fork PRs", false, `octo/docs runs workflows from fork pull requests with secrets; in its settings, under Actions, General, Fork pull request workflows, turn off "Send secrets to workflows from pull requests", or turn off "Run workflows from fork pull requests"`},
			},
		},
		{
			name:    "a repository no class of this box serves",
			repos:   []worker.Repo{{Name: "octo/site", ForkPullRequests: settings(true, false, false, false)}, {Name: "octo/docs", ForkPullRequests: settings(false, false, false, false)}},
			classes: []config.Class{{Name: "boxrunner", Repos: []string{"octo/docs"}}},
			want:    []Result{{"fork PRs", true, "no repository runs workflows from fork pull requests without approval, with write tokens or with secrets"}},
		},
		{
			name:    "some not read yet",
			repos:   []worker.Repo{{Name: "octo/site", ForkPullRequests: settings(false, false, false, false)}, {Name: "octo/docs"}},
			classes: all,
			want:    []Result{{"fork PRs", true, "no repository runs workflows from fork pull requests without approval, with write tokens or with secrets; 1 not read yet"}},
		},
		{
			name:    "none read, or a Worker that does not read them",
			repos:   []worker.Repo{{Name: "octo/site"}},
			classes: all,
			want:    []Result{{"fork PRs", true, "the Worker has not read the repositories' fork pull request settings yet; a Worker older than this check never does"}},
		},
		{name: "no repositories", classes: all},
	}
	for _, c := range cases {
		if got := Forks(c.repos, c.classes); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: Forks =\n%+v\nwant\n%+v", c.name, got, c.want)
		}
	}
}

func TestCache(t *testing.T) {
	url := "http://172.18.0.1:5001"
	for logs, want := range map[string]Result{
		"host 124\r\ncache-url http://172.18.0.1:5001/\r\n": {"actions cache", true, "actions in a job reach the cache at http://172.18.0.1:5001"},
		"cache-url https://results-receiver.actions.githubusercontent.com/\r\n": {"actions cache", false,
			"http://172.18.0.1:5001 did not answer a job container, so jobs use GitHub's cache; see journalctl -u boxrunner.service"},
		"cache-url \r\n": {"actions cache", false, "the job image has no node wrapper for the cache yet; the agent rebuilds the image when no normal-priority job runs"},
	} {
		if got := Cache(logs, url); got != want {
			t.Errorf("Cache(%q) =\n%+v\nwant\n%+v", logs, got, want)
		}
	}
}

func TestOrganization(t *testing.T) {
	if got := Organization(worker.Organization{Name: "octo", Group: "boxrunner"}, 1).Detail; !strings.HasPrefix(got, "the Worker serves 1 private repository of octo ") {
		t.Errorf("Organization with one repository = %q", got)
	}
	ok := Organization(worker.Organization{Name: "octo", Group: "boxrunner"}, 2)
	if want := (Result{"organization", true, `the Worker serves 2 private repositories of octo through one scale set per class in the runner group "boxrunner", which admits only selected repositories and no public one`}); ok != want {
		t.Errorf("Organization = %+v, want %+v", ok, want)
	}
	problem := "The runner group “boxrunner” of octo allows public repositories; turn off “Allow public repositories” in its settings."
	if got, want := Organization(worker.Organization{Name: "octo", Group: "boxrunner", Problem: problem}, 0), (Result{"organization", false, problem}); got != want {
		t.Errorf("Organization = %+v, want %+v", got, want)
	}
}

func TestOrgForks(t *testing.T) {
	want := Result{"fork PRs", true, `cannot check them: reading the fork pull request settings takes the permission Administration, which the App serving an organization does not have. Check "Fork pull request workflows in private repositories" at https://github.com/organizations/octo/settings/actions and in each served repository's Settings, Actions, General`}
	if got := OrgForks("octo"); got != want {
		t.Errorf("OrgForks = %+v, want %+v", got, want)
	}
}

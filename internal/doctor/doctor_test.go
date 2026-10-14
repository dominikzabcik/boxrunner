package doctor

import (
	"errors"
	"net/netip"
	"reflect"
	"testing"

	"github.com/dominikzabcik/boxrunner/internal/config"
	"github.com/dominikzabcik/boxrunner/internal/status"
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

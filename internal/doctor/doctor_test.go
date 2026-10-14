package doctor

import (
	"net/netip"
	"reflect"
	"testing"
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

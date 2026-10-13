package doctor

import (
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

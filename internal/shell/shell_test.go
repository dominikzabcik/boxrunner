package shell

import (
	"reflect"
	"testing"
)

const listed = "box-1-boxrunner-3f9a2c\trunning\tocto/site\tboxrunner\tUp 4 minutes\n" +
	"box-1-boxrunner-light-61d0aa\texited\tocto/api\tboxrunner-light\tExited (1) 2 minutes ago\n"

func TestParseAndPick(t *testing.T) {
	jobs := Parse(listed)
	want := []Job{
		{Name: "box-1-boxrunner-3f9a2c", State: "running", Repo: "octo/site", Class: "boxrunner", Status: "Up 4 minutes"},
		{Name: "box-1-boxrunner-light-61d0aa", State: "exited", Repo: "octo/api", Class: "boxrunner-light", Status: "Exited (1) 2 minutes ago"},
	}
	if !reflect.DeepEqual(jobs, want) {
		t.Fatalf("Parse = %+v", jobs)
	}
	if j, err := Pick(jobs, "3f9a"); err != nil || j.Name != "box-1-boxrunner-3f9a2c" || !j.Running() {
		t.Errorf("Pick by part = %+v, %v", j, err)
	}
	if _, err := Pick(jobs, "boxrunner"); err == nil || err.Error() != `"boxrunner" matches box-1-boxrunner-3f9a2c, box-1-boxrunner-light-61d0aa; use more of the name` {
		t.Errorf("ambiguous Pick error = %v", err)
	}
	if _, err := Pick(jobs, "zzz"); err == nil || err.Error() != `no job container matches "zzz"; run boxrunner shell to list them` {
		t.Errorf("missing Pick error = %v", err)
	}
	if got := Parse(""); got != nil {
		t.Errorf("Parse(empty) = %v", got)
	}
}

// A copy of a stopped job runs under the job's runtime, so a Sysbox job's
// leftovers never run as the host's root.
func TestInspectKeepsTheJobsRuntime(t *testing.T) {
	base := []string{"run", "-it", "--rm", "--network", "boxrunner", "--cpus", "1", "--memory", "1g", "--pids-limit", "512"}
	tail := []string{"-u", "runner", "-w", "/home/runner/_work", "--entrypoint", "bash", "boxrunner/inspect:x"}
	for runtime, extra := range map[string][]string{
		"sysbox-runc": {"--runtime", "sysbox-runc"},
		"runc":        nil,
		"":            nil,
	} {
		want := append(append(append([]string{}, base...), extra...), tail...)
		if got := Inspect("boxrunner/inspect:x", runtime); !reflect.DeepEqual(got, want) {
			t.Errorf("Inspect(%q) = %q", runtime, got)
		}
	}
}

package bench

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestMeasureRunsEachScenarioAndCleansUp(t *testing.T) {
	var calls []string
	times := map[string]string{"": "1000", "boxrunner_idle.slice": "1020", "system.slice": "4100"}
	slice := ""
	docker := func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, args[0]+" "+strings.Join(args[1:min(4, len(args))], " "))
		switch {
		case args[0] == "run" && args[1] == "-d":
			for i, a := range args {
				if a == "--cgroup-parent" {
					slice = args[i+1]
				}
			}
			return "id\n", nil
		case args[0] == "run":
			return times[slice] + "\n", nil
		case args[0] == "ps":
			if slice == "" {
				return "", nil
			}
			return "a\nb\n", nil
		case args[0] == "rm":
			slice = ""
		}
		return "", nil
	}
	results, err := Measure(context.Background(), docker, "boxrunner/runner:local", 2, 1000, 3)
	if err != nil {
		t.Fatal(err)
	}
	got := []time.Duration{results[0].Median(), results[1].Median(), results[2].Median()}
	want := []time.Duration{time.Second, 1020 * time.Millisecond, 4100 * time.Millisecond}
	if !slices.Equal(got, want) {
		t.Errorf("medians = %v, want %v", got, want)
	}
	busy := 0
	for _, c := range calls {
		if strings.HasPrefix(c, "run -d") {
			busy++
		}
	}
	if busy != 4 || !strings.HasPrefix(calls[len(calls)-1], "ps -q") {
		t.Errorf("busy containers started = %d, calls = %q", busy, calls)
	}
}

func TestReport(t *testing.T) {
	r := []Result{
		{Scenario: Scenarios[0], Runs: []time.Duration{1300 * time.Millisecond, 1319 * time.Millisecond, 1400 * time.Millisecond}},
		{Scenario: Scenarios[1], Runs: []time.Duration{1345 * time.Millisecond, 1340 * time.Millisecond, 1350 * time.Millisecond}},
		{Scenario: Scenarios[2], Runs: []time.Duration{6536 * time.Millisecond, 6500 * time.Millisecond, 6600 * time.Millisecond}},
	}
	want := `4 threads of CPU work in a normal-priority container, median of 3 runs:

alone                             1.319s  -
beside busy idle-priority jobs    1.345s  +2%
beside busy normal-priority jobs  6.536s  +396%
`
	if got := Report(r, 4); got != want {
		t.Errorf("Report =\n%s\nwant\n%s", got, want)
	}
}

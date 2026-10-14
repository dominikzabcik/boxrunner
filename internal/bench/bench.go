// Package bench measures how much a normal-priority job slows down beside
// busy idle-priority jobs, and beside the same load at normal priority, so
// anyone can rerun the number docs/decisions.md reports on their own box.
package bench

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/layout"
)

// Docker runs the docker CLI and returns its standard output.
type Docker func(ctx context.Context, args ...string) (string, error)

// Scenario is what runs beside the timed job.
type Scenario struct {
	Name string
	// Slice is the cgroup parent of the busy containers; "" means none run.
	Slice string
}

var Scenarios = []Scenario{
	{Name: "alone"},
	{Name: "beside busy idle-priority jobs", Slice: layout.IdleSlice},
	{Name: "beside busy normal-priority jobs", Slice: "system.slice"},
}

type Result struct {
	Scenario Scenario
	Runs     []time.Duration
}

func (r Result) Median() time.Duration {
	runs := slices.Sorted(slices.Values(r.Runs))
	return runs[len(runs)/2]
}

// workload runs one CPU-bound process per thread and prints its wall time in
// milliseconds.
const workload = `import sys, time, multiprocessing as mp
def work(_):
    x = 0
    for i in range(int(sys.argv[2])):
        x += i * i
    return x
start = time.perf_counter()
with mp.Pool(int(sys.argv[1])) as pool:
    pool.map(work, range(int(sys.argv[1])))
print(round((time.perf_counter() - start) * 1000))`

const benchLabel = "boxrunner.bench=1"

// Measure times the workload rounds times in each scenario, with as many busy
// containers as threads beside it.
func Measure(ctx context.Context, docker Docker, image string, threads, iterations, rounds int) ([]Result, error) {
	defer cleanup(docker)
	var results []Result
	for _, s := range Scenarios {
		if s.Slice != "" {
			for i := range threads {
				_, err := docker(ctx, "run", "-d", "--rm", "--label", benchLabel,
					"--name", fmt.Sprintf("boxrunner-bench-busy-%d", i), "--cgroup-parent", s.Slice,
					"--entrypoint", "python3", image, "-c", "while True: pass")
				if err != nil {
					return nil, fmt.Errorf("%s: starting a busy container: %w", s.Name, err)
				}
			}
		}
		r := Result{Scenario: s}
		for range rounds {
			out, err := docker(ctx, "run", "--rm", "--label", benchLabel, "--entrypoint", "python3", image,
				"-c", workload, strconv.Itoa(threads), strconv.Itoa(iterations))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", s.Name, err)
			}
			ms, err := strconv.Atoi(strings.TrimSpace(out))
			if err != nil {
				return nil, fmt.Errorf("%s: workload printed %q", s.Name, out)
			}
			r.Runs = append(r.Runs, time.Duration(ms)*time.Millisecond)
		}
		results = append(results, r)
		cleanup(docker)
	}
	return results, nil
}

func cleanup(docker Docker) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ids, err := docker(ctx, "ps", "-q", "--filter", "label="+benchLabel)
	if err != nil || strings.TrimSpace(ids) == "" {
		return
	}
	_, _ = docker(ctx, append([]string{"rm", "-f"}, strings.Fields(ids)...)...)
}

// Report is a table of median times and the slowdown against running alone.
func Report(results []Result, threads int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d threads of CPU work in a normal-priority container, median of %d runs:\n\n", threads, len(results[0].Runs))
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	alone := results[0].Median()
	for _, r := range results {
		m := r.Median()
		slower := "-"
		if r.Scenario.Slice != "" {
			slower = fmt.Sprintf("%+.0f%%", (float64(m)/float64(alone)-1)*100)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", r.Scenario.Name, m.Round(time.Millisecond), slower)
	}
	w.Flush()
	return b.String()
}

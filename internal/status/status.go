// Package status is what `boxrunner status` shows: the running agent's
// snapshot over a unix socket, and the log of finished jobs.
package status

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
)

type Snapshot struct {
	Box       string      `json:"box"`
	Version   string      `json:"version"`
	Classes   []Class     `json:"classes"`
	ScaleSets []ScaleSet  `json:"scaleSets"`
	Runners   []Runner    `json:"runners"`
	Image     ImageStatus `json:"image"`
	DiskFree  int64       `json:"diskFree"`
	DiskFloor int64       `json:"diskFloor"`
	Jobs      []Job       `json:"jobs,omitempty"`
	// Started is when the agent started.
	Started time.Time `json:"started,omitzero"`
	// FreshStart means a box that never ran a job found no containers to
	// adopt, or a session an earlier owner left open: jobs a box that died had
	// acquired may be stranded at GitHub.
	FreshStart bool `json:"freshStart,omitempty"`
}

// strandedHint is how long after a fresh start status points to recovering
// jobs a dead box acquired.
const strandedHint = 30 * time.Minute

type Class struct {
	Name     string `json:"name"`
	Priority string `json:"priority"`
	Max      int    `json:"max"`
	Active   int    `json:"active"`
}

type ScaleSet struct {
	Repo     string `json:"repo"`
	Class    string `json:"class"`
	Assigned int    `json:"assigned"`
}

type Runner struct {
	Name  string    `json:"name"`
	Repo  string    `json:"repo"`
	Class string    `json:"class"`
	Phase string    `json:"phase"`
	Since time.Time `json:"since"`
	// Warm means the runner is kept ready for its repository and is never
	// removed for idleness.
	Warm bool `json:"warm,omitempty"`
}

type ImageStatus struct {
	Built  string `json:"built"`
	Latest string `json:"latest"`
}

// Job is one line of the job log.
type Job struct {
	Repo     string    `json:"repo"`
	Class    string    `json:"class"`
	Runner   string    `json:"runner"`
	Workflow string    `json:"workflow,omitempty"`
	Name     string    `json:"name,omitempty"`
	Result   string    `json:"result"`
	Queued   time.Time `json:"queued,omitzero"`
	Started  time.Time `json:"started,omitzero"`
	Finished time.Time `json:"finished"`
}

// Wait is how long the job queued before a runner took it.
func (j Job) Wait() time.Duration { return span(j.Queued, j.Started) }

// Took is how long the job ran.
func (j Job) Took() time.Duration { return span(j.Started, j.Finished) }

func span(from, to time.Time) time.Duration {
	if from.IsZero() || to.IsZero() || to.Before(from) {
		return 0
	}
	return to.Sub(from)
}

// waitText shows a queue wait, or "?" for a job logged before boxrunner knew
// when it was queued.
func waitText(j Job) string {
	if j.Queued.IsZero() {
		return "?"
	}
	return j.Wait().Round(time.Second).String()
}

const maxLog = 1 << 20

// Append adds a job to the log, dropping the older half when it grows past a
// megabyte.
func Append(path string, j Job) error {
	line, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	size := int64(0)
	if info, statErr := f.Stat(); statErr == nil {
		size = info.Size()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil || size <= maxLog {
		return err
	}
	jobs, err := Recent(path, math.MaxInt)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	for _, j := range jobs[len(jobs)/2:] {
		b, _ := json.Marshal(j)
		buf.Write(append(b, '\n'))
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Recent returns the newest n jobs, oldest first. A missing log has none.
func Recent(path string, n int) ([]Job, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var jobs []Job
	s := bufio.NewScanner(f)
	for s.Scan() {
		var j Job
		if json.Unmarshal(s.Bytes(), &j) == nil {
			jobs = append(jobs, j)
		}
	}
	return jobs[max(0, len(jobs)-n):], s.Err()
}

// Serve answers GET /status on a unix socket until ctx ends. snapshot is
// called for every request.
func Serve(ctx context.Context, socket string, snapshot func() Snapshot) error {
	os.Remove(socket)
	l, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		l.Close()
		return err
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(snapshot())
	})}
	go func() {
		<-ctx.Done()
		srv.Close()
		os.Remove(socket)
	}()
	if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Fetch asks the running agent for its snapshot.
func Fetch(ctx context.Context, socket string) (Snapshot, error) {
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://boxrunner/status", nil)
	resp, err := client.Do(req)
	if err != nil {
		return Snapshot{}, fmt.Errorf("the agent is not answering on %s (is boxrunner.service running?): %w", socket, err)
	}
	defer resp.Body.Close()
	var s Snapshot
	return s, json.NewDecoder(resp.Body).Decode(&s)
}

// Print writes a snapshot for people.
func Print(w io.Writer, s Snapshot, now time.Time) {
	fmt.Fprintf(w, "%s  boxrunner %s  image %s (latest %s)  disk %s free, floor %s\n\n",
		s.Box, s.Version, orNone(s.Image.Built), orNone(s.Image.Latest), gigabytes(s.DiskFree), gigabytes(s.DiskFloor))
	if s.FreshStart && now.Sub(s.Started) < strandedHint {
		fmt.Fprint(w, "If this box replaced one that died, jobs the old box had acquired stay queued: see docs/architecture.md#replacing-a-box.\n\n")
	}
	t := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(t, "CLASS\tPRIORITY\tRUNNERS\tMAX")
	for _, c := range s.Classes {
		fmt.Fprintf(t, "%s\t%s\t%d\t%d\n", c.Name, c.Priority, c.Active, c.Max)
	}
	t.Flush()

	fmt.Fprintln(w)
	fmt.Fprintln(t, "REPOSITORY\tCLASS\tASSIGNED JOBS")
	for _, ss := range s.ScaleSets {
		fmt.Fprintf(t, "%s\t%s\t%d\n", ss.Repo, ss.Class, ss.Assigned)
	}
	t.Flush()

	fmt.Fprintln(w)
	if len(s.Runners) == 0 {
		fmt.Fprintln(w, "No runners.")
	} else {
		fmt.Fprintln(t, "RUNNER\tREPOSITORY\tCLASS\tSTATE\tFOR")
		for _, r := range s.Runners {
			state := r.Phase
			if r.Warm {
				state += " (warm)"
			}
			fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\n", r.Name, r.Repo, r.Class, state, now.Sub(r.Since).Round(time.Second))
		}
		t.Flush()
	}

	fmt.Fprintln(w)
	if len(s.Jobs) == 0 {
		fmt.Fprintln(w, "No finished jobs yet.")
		return
	}
	fmt.Fprintln(t, "FINISHED\tREPOSITORY\tCLASS\tJOB\tRESULT\tQUEUED FOR\tTOOK")
	for _, j := range s.Jobs {
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", j.Finished.Local().Format("Jan 02 15:04"), j.Repo, j.Class,
			orNone(j.Name), j.Result, waitText(j), j.Took().Round(time.Second))
	}
	t.Flush()
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "none"
	}
	return s
}

func gigabytes(b int64) string { return fmt.Sprintf("%.1f GB", float64(b)/1e9) }

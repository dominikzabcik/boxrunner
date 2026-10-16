// Package status is what `boxrunner status` shows: the running agent's
// snapshot over a unix socket, and the log of finished jobs.
package status

import (
	"bufio"
	"bytes"
	"cmp"
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
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/egress"
	"github.com/dominikzabcik/boxrunner/internal/release"
)

type Snapshot struct {
	Box       string      `json:"box"`
	Version   string      `json:"version"`
	Classes   []Class     `json:"classes"`
	ScaleSets []ScaleSet  `json:"scaleSets"`
	Elsewhere []Elsewhere `json:"elsewhere,omitempty"`
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
	// Worker is the Worker's release, empty before it answered or when it
	// names none.
	Worker string `json:"worker,omitempty"`
	// Latest is boxrunner's latest release, once the agent has found it.
	Latest string `json:"latest,omitempty"`
	// Draining means `boxrunner drain` closed the sessions: the agent starts
	// no runner until `boxrunner resume`.
	Draining bool `json:"draining,omitempty"`
	// Finished counts the jobs that finished since the agent started.
	Finished []Tally `json:"finished,omitempty"`
	// Heartbeat is when the Worker last took a heartbeat.
	Heartbeat time.Time `json:"heartbeat,omitzero"`
	// Egress is the state of each class that has an allowlist.
	Egress []Egress `json:"egress,omitempty"`
}

// Egress is what the agent knows of one class with an egress allowlist.
type Egress struct {
	Class string `json:"class"`
	// Entries is the allowlist as written.
	Entries []string `json:"entries"`
	// Problem says why the agent does not serve the class, "" when it does.
	Problem string `json:"problem,omitempty"`
	// Packets and Lookups are what the class's jobs were refused since the
	// agent started: packets dropped, and names refused.
	Packets uint64 `json:"packets"`
	Lookups uint64 `json:"lookups"`
	// Recent are the latest refusals the agent recorded, oldest first.
	Recent []egress.Block `json:"recent,omitempty"`
}

// Summary says what the class was refused, such as "12 packets dropped and 1
// lookup refused since the agent started".
func (e Egress) Summary() string {
	return fmt.Sprintf("%s dropped and %s refused since the agent started", plural(e.Packets, "packet"), plural(e.Lookups, "lookup"))
}

func plural(n uint64, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// Drained is true once a draining agent has closed every session and no job
// container runs or is about to.
func (s Snapshot) Drained() bool {
	return s.Draining && len(s.ScaleSets) == 0 && len(s.Busy()) == 0
}

// Busy are the runners whose container runs or is about to.
func (s Snapshot) Busy() []Runner {
	var busy []Runner
	for _, r := range s.Runners {
		if r.Phase != "exited" {
			busy = append(busy, r)
		}
	}
	return busy
}

// strandedHint is how long after a fresh start status points to recovering
// jobs a dead box acquired.
const strandedHint = 30 * time.Minute

type Class struct {
	Name     string `json:"name"`
	Priority string `json:"priority"`
	Max      int    `json:"max"`
	Active   int    `json:"active"`
	// HeldUntil is set while the class's runners fail before taking a job:
	// it starts no runner before then, and one at a time after.
	HeldUntil time.Time `json:"heldUntil,omitzero"`
}

// NextTry says when a class that holds its starts tries again, or "" when
// it does not hold.
func (c Class) NextTry(now time.Time) string {
	switch {
	case c.HeldUntil.IsZero():
		return ""
	case now.Before(c.HeldUntil):
		return "next try at " + c.HeldUntil.Local().Format("15:04:05")
	default:
		return "trying one runner now"
	}
}

type ScaleSet struct {
	Repo     string `json:"repo"`
	Class    string `json:"class"`
	Assigned int    `json:"assigned"`
}

// Elsewhere is a repository and class this box is configured for that
// another box serves, because that box's session holds the scale set.
type Elsewhere struct {
	Repo  string `json:"repo"`
	Class string `json:"class"`
	Box   string `json:"box"`
}

// Sentence says which box serves the repository and class, and how to fix it.
func (e Elsewhere) Sentence() string {
	return fmt.Sprintf("%s serves %s %s; this box takes its jobs only after %s stops. Give each box its own classes or repos (docs/config.md#several-boxes).", e.Box, e.Repo, e.Class, e.Box)
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
	Built string `json:"built"`
	// Digest is the runner image's that Built is built from, empty for a job
	// image built before boxrunner built from the digests its CI verified.
	Digest string `json:"digest,omitempty"`
	Latest string `json:"latest"`
	// Unlisted is the runner's latest release while boxrunner's CI has not
	// verified its image; Latest is then an older release.
	Unlisted string `json:"unlisted,omitempty"`
	// Refused says why the agent last refused to build a runner image, until
	// a build succeeds.
	Refused string `json:"refused,omitempty"`
	// Disk is what the job, class and runner images take, 0 when unknown.
	Disk int64 `json:"disk,omitempty"`
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
func (j Job) Wait() time.Duration {
	d, _ := span(j.Queued, j.Started)
	return d
}

// Took is how long the job ran.
func (j Job) Took() time.Duration {
	d, _ := span(j.Started, j.Finished)
	return d
}

// span is the time from one moment to a later one, and false when either is
// unknown or they are out of order.
func span(from, to time.Time) (time.Duration, bool) {
	if from.IsZero() || to.IsZero() || to.Before(from) {
		return 0, false
	}
	return to.Sub(from), true
}

// Tally counts the finished jobs of one repository, class and result.
type Tally struct {
	Repo   string `json:"repo"`
	Class  string `json:"class"`
	Result string `json:"result"`
	Jobs   uint64 `json:"jobs"`
	// Took and Wait leave out the jobs whose times GitHub did not send.
	Took Histogram `json:"took"`
	Wait Histogram `json:"wait"`
}

// Buckets are the upper bounds, in seconds, of a Histogram's buckets: from a
// warm runner's start to GitHub's six-hour limit on a job.
var Buckets = [...]float64{1, 5, 10, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200, 21600}

// Histogram counts durations in Buckets. Counts[i] holds those over the
// bound before Buckets[i] and up to it; the last holds those over every
// bound.
type Histogram struct {
	Counts [len(Buckets) + 1]uint64 `json:"counts"`
	// Sum is in seconds.
	Sum float64 `json:"sum"`
}

func (h *Histogram) add(d time.Duration) {
	i, _ := slices.BinarySearch(Buckets[:], d.Seconds())
	h.Counts[i]++
	h.Sum += d.Seconds()
}

// Count returns the tallies, sorted by repository, class and result, with j
// counted. It leaves tallies as they were, so a snapshot that holds them can
// be read while the agent counts the next job.
func Count(tallies []Tally, j Job) []Tally {
	i, found := slices.BinarySearchFunc(tallies, j, func(t Tally, j Job) int {
		return cmp.Or(cmp.Compare(t.Repo, j.Repo), cmp.Compare(t.Class, j.Class), cmp.Compare(t.Result, j.Result))
	})
	tallies = slices.Clone(tallies)
	if !found {
		tallies = slices.Insert(tallies, i, Tally{Repo: j.Repo, Class: j.Class, Result: j.Result})
	}
	t := &tallies[i]
	t.Jobs++
	if d, ok := span(j.Started, j.Finished); ok {
		t.Took.add(d)
	}
	if d, ok := span(j.Queued, j.Started); ok {
		t.Wait.add(d)
	}
	return tallies
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

// Serve answers GET /status, POST /drain and POST /resume on a unix socket
// until ctx ends, and returns once the socket file is gone. snapshot is called
// for every status request, and drain with true to drain and false to resume.
func Serve(ctx context.Context, socket string, snapshot func() Snapshot, drain func(bool) error) error {
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
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/status":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(snapshot())
		case r.Method == http.MethodPost && (r.URL.Path == "/drain" || r.URL.Path == "/resume"):
			if err := drain(r.URL.Path == "/drain"); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})}
	stop := context.AfterFunc(ctx, func() { srv.Close() })
	defer stop()
	if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Fetch asks the running agent for its snapshot.
func Fetch(ctx context.Context, socket string) (Snapshot, error) {
	resp, err := call(ctx, socket, http.MethodGet, "/status")
	if err != nil {
		return Snapshot{}, err
	}
	defer resp.Body.Close()
	var s Snapshot
	return s, json.NewDecoder(resp.Body).Decode(&s)
}

// Drain asks the running agent to drain, or with on false to resume.
func Drain(ctx context.Context, socket string, on bool) error {
	path := map[bool]string{true: "/drain", false: "/resume"}[on]
	resp, err := call(ctx, socket, http.MethodPost, path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("the agent refused %s: %s", path, strings.TrimSpace(string(body)))
	}
	return nil
}

func call(ctx context.Context, socket, method, path string) (*http.Response, error) {
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
	req, _ := http.NewRequestWithContext(ctx, method, "http://boxrunner"+path, nil)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the agent is not answering on %s (is boxrunner.service running?): %w", socket, err)
	}
	return resp, nil
}

// Print writes a snapshot for people.
func Print(w io.Writer, s Snapshot, now time.Time) {
	images := ""
	if s.Image.Disk > 0 {
		images = "  images " + gigabytes(s.Image.Disk)
	}
	fmt.Fprintf(w, "%s  boxrunner %s  worker %s  image %s (latest %s)%s  disk %s free, floor %s\n\n",
		s.Box, s.Version, cmp.Or(s.Worker, "unknown"), orNone(s.Image.Built), orNone(s.Image.Latest), images, gigabytes(s.DiskFree), gigabytes(s.DiskFloor))
	if release.Newer(s.Latest, s.Version) {
		fmt.Fprintf(w, "boxrunner %s is out. Upgrade this agent with: sudo boxrunner upgrade\n\n", s.Latest)
	}
	if release.Newer(s.Latest, s.Worker) {
		fmt.Fprintf(w, "The Worker runs %s. Redeploy it from %s: see docs/upgrading.md.\n\n", s.Worker, s.Latest)
	}
	if s.Image.Digest != "" {
		fmt.Fprintf(w, "The job image is built from runner image %s, which boxrunner's CI verified.\n\n", s.Image.Digest)
	}
	if s.Image.Unlisted != "" {
		building, _, _ := strings.Cut(s.Image.Latest, "+")
		fmt.Fprintf(w, "Runner %s is out, but boxrunner's CI has not verified its image yet, so the agent builds %s until it does.\n\n", s.Image.Unlisted, building)
	}
	if s.Image.Refused != "" {
		fmt.Fprintf(w, "The agent refused to build the job image, and jobs keep the current image: %s\n\n", s.Image.Refused)
	}
	if s.Draining {
		fmt.Fprint(w, "Draining: the agent closed its sessions and starts no runners; running jobs finish. Resume with: sudo boxrunner resume\n\n")
	}
	if s.FreshStart && now.Sub(s.Started) < strandedHint {
		fmt.Fprint(w, "If this box replaced one that died, jobs the old box had acquired stay queued: see docs/architecture.md#replacing-a-box.\n\n")
	}
	for _, c := range s.Classes {
		if next := c.NextTry(now); next != "" {
			fmt.Fprintf(w, "Runners of class %s exit before taking a job; %s. See `journalctl -u boxrunner` for their output.\n\n", c.Name, next)
		}
	}
	for _, e := range s.Egress {
		if e.Problem != "" {
			fmt.Fprintf(w, "Class %s has an egress allowlist and is not served: %s\n\n", e.Class, e.Problem)
		}
	}
	t := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(t, "CLASS\tPRIORITY\tRUNNERS\tMAX")
	for _, c := range s.Classes {
		fmt.Fprintf(t, "%s\t%s\t%d\t%d\n", c.Name, c.Priority, c.Active, c.Max)
	}
	t.Flush()
	if len(s.Egress) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(t, "EGRESS CLASS\tENTRIES\tPACKETS DROPPED\tLOOKUPS REFUSED")
		for _, e := range s.Egress {
			fmt.Fprintf(t, "%s\t%d\t%d\t%d\n", e.Class, len(e.Entries), e.Packets, e.Lookups)
		}
		t.Flush()
		type refusal struct {
			class string
			egress.Block
		}
		var recent []refusal
		for _, e := range s.Egress {
			for _, b := range e.Recent {
				recent = append(recent, refusal{e.Class, b})
			}
		}
		if len(recent) > 0 {
			slices.SortStableFunc(recent, func(a, b refusal) int { return a.Time.Compare(b.Time) })
			fmt.Fprintln(w)
			fmt.Fprintln(t, "REFUSED AT\tCLASS\tRUNNER\tREPOSITORY\tWHAT")
			for _, r := range recent[max(0, len(recent)-20):] {
				fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\n", r.Time.Local().Format("15:04:05"), r.class, cmp.Or(r.Runner, "?"), cmp.Or(r.Repo, "?"), r.Block.String())
			}
			t.Flush()
		}
	}

	fmt.Fprintln(w)
	fmt.Fprintln(t, "REPOSITORY\tCLASS\tASSIGNED JOBS")
	for _, ss := range s.ScaleSets {
		fmt.Fprintf(t, "%s\t%s\t%d\n", ss.Repo, ss.Class, ss.Assigned)
	}
	t.Flush()
	if len(s.Elsewhere) > 0 {
		fmt.Fprintln(w)
	}
	for _, e := range s.Elsewhere {
		fmt.Fprintln(w, e.Sentence())
	}

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

// Package worker is the agent's client for its Worker (docs/api.md).
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/backoff"
	"github.com/dominikzabcik/boxrunner/internal/queue"
	"github.com/dominikzabcik/boxrunner/internal/status"
)

// Code is the "error" field of a Worker error.
type Code string

const (
	BadRequest      Code = "bad_request"
	UnknownClass    Code = "unknown_class"
	Unauthorized    Code = "unauthorized"
	NotFound        Code = "not_found"
	SessionExists   Code = "session_exists"
	RunnerBusy      Code = "runner_busy"
	GroupIneligible Code = "group_ineligible"
	RepoIneligible  Code = "repo_ineligible"
	UpgradeRequired Code = "upgrade_required"
	RateLimited     Code = "rate_limited"
	GitHubError     Code = "github_error"
	NotConfigured   Code = "not_configured"
)

// handling is what the agent does with each code. Codes the client retries
// never reach a caller unless its context ends first.
type handling int

const (
	giveBack handling = iota // the caller decides
	retry
	fatal // the agent stops; a restart cannot fix it
)

var handlings = map[Code]handling{
	BadRequest:      giveBack,
	UnknownClass:    giveBack,
	Unauthorized:    fatal,
	NotFound:        giveBack,
	SessionExists:   retry,
	RunnerBusy:      giveBack,
	GroupIneligible: giveBack,
	RepoIneligible:  giveBack,
	UpgradeRequired: fatal,
	RateLimited:     retry,
	GitHubError:     retry,
	NotConfigured:   fatal,
}

// Error is a Worker answer outside 2xx.
type Error struct {
	Status  int
	Code    Code
	Message string
	Call    string
	// HeldBy is the other box whose session holds the scale set, with SessionExists.
	HeldBy string
}

func (e *Error) Error() string {
	code := string(e.Code)
	if code == "" {
		code = "no error code"
	}
	return fmt.Sprintf("worker %s: %d %s: %s", e.Call, e.Status, code, e.Message)
}

func (e *Error) handling() handling {
	if e.HeldBy != "" {
		// Another box serves the scale set; the caller tries again later.
		return giveBack
	}
	if h, ok := handlings[e.Code]; ok {
		return h
	}
	if e.Status >= 500 || e.Status == http.StatusTooManyRequests {
		return retry
	}
	return giveBack
}

// Is reports whether err is a Worker error with this code.
func Is(err error, code Code) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// HeldBy is the other box whose session holds the scale set err was about,
// or "" when err is not that.
func HeldBy(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.HeldBy
	}
	return ""
}

// SessionBlocked reports whether a session an earlier owner left open ever
// refused a new one.
func (c *Client) SessionBlocked() bool { return c.blocked.Load() }

// WorkerVersion is the release the Worker named in its latest answer, and
// whether it has answered at all. A Worker deployed before Workers named
// their release answers with an empty version.
func (c *Client) WorkerVersion() (version string, answered bool) {
	if v := c.workerVersion.Load(); v != nil {
		return *v, true
	}
	return "", false
}

// Fatal reports whether err means the agent must stop: a wrong token, an
// agent too old for its Worker, or a Worker whose setup is unfinished.
func Fatal(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.handling() == fatal
}

type Repo struct {
	Name string `json:"name"`
	ID   int64  `json:"id"`
	// ForkPullRequests is nil until the Worker has read the settings, and
	// from a Worker that does not read them.
	ForkPullRequests *ForkPullRequests `json:"forkPullRequests"`
}

// ForkPullRequests are a private repository's settings for workflows from
// fork pull requests, under Settings, Actions, General.
type ForkPullRequests struct {
	RunWorkflows    bool `json:"runWorkflows"`
	RequireApproval bool `json:"requireApproval"`
	SendWriteTokens bool `json:"sendWriteTokens"`
	SendSecrets     bool `json:"sendSecrets"`
}

type Repos struct {
	Repos []Repo `json:"repos"`
	// Organization is set when the Worker serves an organization through one
	// scale set per class; Repos are then the repositories its runner group
	// admits. A Worker that serves repositories one by one sends none.
	Organization *Organization `json:"organization"`
	// Classes are the classes to serve now: the Worker's classes without the paused ones.
	Classes []string `json:"classes"`
	// Paused are the classes paused on the dashboard. A Worker from before pausing sends none.
	Paused []string `json:"paused"`
}

// Organization is the organization a Worker serves and the runner group its
// scale sets are in. Problem says why the Worker serves none of its
// repositories, such as a group that admits public repositories.
type Organization struct {
	Name    string `json:"name"`
	Group   string `json:"group"`
	Problem string `json:"problem"`
}

// IsOrg reports whether a scale set's scope, which the agent keeps where a
// repository's name goes, names an organization rather than a repository.
func IsOrg(scope string) bool { return scope != "" && !strings.Contains(scope, "/") }

// scopeField is the field that names a scale set's scope in a request.
func scopeField(scope string) string {
	if IsOrg(scope) {
		return "org"
	}
	return "repo"
}

type ScaleSet struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type Session struct {
	ID         string
	Queue      queue.Session
	Statistics queue.Statistics
}

type Runner struct {
	ID        int64  `json:"runnerId"`
	JITConfig string `json:"jitConfig"`
}

type Client struct {
	base    *url.URL
	token   string
	version string
	http    *http.Client
	backoff backoff.Policy
	log     *slog.Logger
	// blocked is set once a session an earlier owner left open refused a new one.
	blocked atomic.Bool
	// workerVersion is the release the Worker named in its latest answer.
	workerVersion atomic.Pointer[string]
}

type Options struct {
	URL     *url.URL
	Token   string
	Version string
	HTTP    *http.Client
	Backoff backoff.Policy
	Log     *slog.Logger
}

func New(o Options) *Client {
	c := &Client{base: o.URL, token: o.Token, version: o.Version, http: o.HTTP, backoff: o.Backoff, log: o.Log}
	if c.http == nil {
		c.http = &http.Client{Timeout: 60 * time.Second}
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	return c
}

// Heartbeat is the box's state for the dashboard (docs/api.md).
type Heartbeat struct {
	Box     string            `json:"box"`
	Version string            `json:"version"`
	Image   string            `json:"image"`
	Disk    HeartbeatDisk     `json:"disk"`
	Classes []HeartbeatClass  `json:"classes"`
	Runners []HeartbeatRunner `json:"runners"`
	Jobs    []HeartbeatJob    `json:"jobs"`
	// Latest is boxrunner's latest release, once the agent has found it.
	Latest string `json:"latest,omitempty"`
	// Draining and ScaleSets tell the Worker why jobs may wait with no
	// runner, for its alerts. A Worker from before them ignores them.
	Draining  bool                `json:"draining,omitempty"`
	ScaleSets []HeartbeatScaleSet `json:"scaleSets,omitempty"`
	// Rank orders the boxes that serve one class for the Worker's fleet shares.
	Rank int `json:"rank,omitempty"`
}

type HeartbeatDisk struct {
	Free  int64 `json:"free"`
	Floor int64 `json:"floor"`
}

type HeartbeatClass struct {
	Name     string `json:"name"`
	Priority string `json:"priority"`
	Max      int    `json:"max"`
	// HeldUntil is set while the class's runners fail before taking a job.
	HeldUntil time.Time `json:"heldUntil,omitzero"`
}

// HeartbeatScaleSet is a scale set the box listens to and its assigned
// jobs, including the ones runners already took. A count of 0 tells the
// Worker that the scale set's jobs no longer wait.
type HeartbeatScaleSet struct {
	Repo     string `json:"repo"`
	Class    string `json:"class"`
	Assigned int    `json:"assigned"`
	// Pending names the repositories of an organization scale set's jobs that
	// no runner took, which other boxes name when they mint runners for them.
	Pending []string `json:"pending,omitempty"`
}

type HeartbeatRunner struct {
	Name  string    `json:"name"`
	Repo  string    `json:"repo"`
	Class string    `json:"class"`
	State string    `json:"state"`
	Since time.Time `json:"since"`
}

type HeartbeatJob struct {
	Runner   string     `json:"runner"`
	Repo     string     `json:"repo"`
	Class    string     `json:"class"`
	Name     string     `json:"name"`
	Result   string     `json:"result"`
	Queued   *time.Time `json:"queued"`
	Started  *time.Time `json:"started"`
	Finished time.Time  `json:"finished"`
	// Usage is what the job's container used; a Worker from before it
	// ignores it.
	Usage *status.Usage `json:"usage,omitempty"`
}

// HeartbeatReply is the Worker's answer to a heartbeat. A Worker from before
// pausing answers 204, which leaves it empty.
type HeartbeatReply struct {
	Paused []string `json:"paused"`
	// Fleet is the box's share of each scale set of a class that another box
	// serves too; nil while no class of the box has one, and from a Worker
	// from before fleets.
	Fleet *[]FleetShare `json:"fleet"`
	// Limits caps classes on this box below their max, as set for the box on
	// the dashboard; empty from a Worker from before limits.
	Limits map[string]int `json:"limits"`
}

// FleetShare is how many of a scale set's jobs the box's runners should
// hold, and for an organization scale set the repositories of the waiting ones.
type FleetShare struct {
	Repo  string   `json:"repo"`
	Class string   `json:"class"`
	Jobs  int      `json:"jobs"`
	Repos []string `json:"repos"`
}

// SendHeartbeat makes one attempt: the next heartbeat follows anyway.
func (c *Client) SendHeartbeat(ctx context.Context, hb Heartbeat) (HeartbeatReply, error) {
	var out HeartbeatReply
	body, err := json.Marshal(hb)
	if err != nil {
		return out, err
	}
	err = c.once(ctx, "heartbeat", http.MethodPost, c.base.JoinPath("/v1/heartbeat").String(), body, &out)
	return out, err
}

func (c *Client) Repos(ctx context.Context) (Repos, error) {
	var out Repos
	err := c.do(ctx, "list repositories", http.MethodGet, "/v1/repos", nil, nil, &out)
	return out, err
}

// ScaleSet finds or creates the scale set of a class for repo, which is
// owner/name, or for an organization scale set the organization's login. So
// do the other calls that name a scale set.
func (c *Client) ScaleSet(ctx context.Context, repo, class string) (ScaleSet, error) {
	var out ScaleSet
	err := c.do(ctx, "find scale set", http.MethodPost, "/v1/scalesets", nil,
		map[string]any{scopeField(repo): repo, "class": class}, &out)
	return out, err
}

type sessionResponse struct {
	SessionID      string           `json:"sessionId"`
	QueueURL       string           `json:"queueUrl"`
	QueueToken     string           `json:"queueToken"`
	AcquireJobsURL string           `json:"acquireJobsUrl"`
	Statistics     queue.Statistics `json:"statistics"`
}

func (r sessionResponse) session() Session {
	return Session{
		ID:         r.SessionID,
		Queue:      queue.Session{URL: r.QueueURL, Token: r.QueueToken, AcquireJobsURL: r.AcquireJobsURL},
		Statistics: r.Statistics,
	}
}

// OpenSession opens the scale set's message session. While an earlier session
// of this box still holds the scale set it keeps retrying; GitHub releases an
// abandoned one after about a minute. When another box's session holds it,
// OpenSession returns at once with an error whose HeldBy names that box.
func (c *Client) OpenSession(ctx context.Context, repo string, scaleSetID int64, owner string) (Session, error) {
	var out sessionResponse
	err := c.do(ctx, "open session", http.MethodPost, "/v1/sessions", nil,
		map[string]any{scopeField(repo): repo, "scaleSetId": scaleSetID, "owner": owner}, &out)
	return out.session(), err
}

// RefreshSession renews a session's queue token. It names the owner so a
// Worker that started keeping session holders after the session opened learns it.
func (c *Client) RefreshSession(ctx context.Context, repo string, scaleSetID int64, sessionID, owner string) (Session, error) {
	var out sessionResponse
	err := c.do(ctx, "refresh session", http.MethodPatch, "/v1/sessions/"+url.PathEscape(sessionID), nil,
		map[string]any{scopeField(repo): repo, "scaleSetId": scaleSetID, "owner": owner}, &out)
	return out.session(), err
}

// CloseSession closes a session; one already gone counts as closed.
func (c *Client) CloseSession(ctx context.Context, repo string, scaleSetID int64, sessionID string) error {
	err := c.do(ctx, "close session", http.MethodDelete, "/v1/sessions/"+url.PathEscape(sessionID),
		scaleSetQuery(repo, scaleSetID), nil, nil)
	if Is(err, NotFound) {
		return nil
	}
	return err
}

// MintRunner mints a one-job runner. For an organization scale set, jobRepos
// are the repositories of the jobs it holds that no runner has taken: the
// Worker refuses the runner while one of them is not a repository it serves.
func (c *Client) MintRunner(ctx context.Context, repo string, scaleSetID int64, name string, jobRepos []string) (Runner, error) {
	var out Runner
	in := map[string]any{scopeField(repo): repo, "scaleSetId": scaleSetID, "name": name}
	if IsOrg(repo) {
		in["jobRepos"] = append([]string{}, jobRepos...)
	}
	err := c.do(ctx, "mint runner", http.MethodPost, "/v1/runners", nil, in, &out)
	return out, err
}

// AcquirableJobs lists the jobs an organization scale set may still acquire,
// of repositories the Worker serves. GitHub offers a job in one message, so
// one the agent turned down is found here once it serves the repository.
func (c *Client) AcquirableJobs(ctx context.Context, repo string, scaleSetID int64) ([]queue.Job, error) {
	var out struct {
		Jobs []queue.Job `json:"jobs"`
	}
	err := c.do(ctx, "list acquirable jobs", http.MethodGet, "/v1/jobs", scaleSetQuery(repo, scaleSetID), nil, &out)
	return out.Jobs, err
}

// RemoveRunner removes a runner that has not taken a job. One already gone
// counts as removed; one running a job answers with RunnerBusy.
func (c *Client) RemoveRunner(ctx context.Context, repo string, scaleSetID, runnerID int64) error {
	err := c.do(ctx, "remove runner", http.MethodDelete, "/v1/runners/"+strconv.FormatInt(runnerID, 10),
		scaleSetQuery(repo, scaleSetID), nil, nil)
	if Is(err, NotFound) {
		return nil
	}
	return err
}

func scaleSetQuery(repo string, scaleSetID int64) url.Values {
	return url.Values{scopeField(repo): {repo}, "scaleSetId": {strconv.FormatInt(scaleSetID, 10)}}
}

func (c *Client) do(ctx context.Context, call, method, path string, query url.Values, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	u := c.base.JoinPath(path)
	u.RawQuery = query.Encode()
	for attempt := 0; ; attempt++ {
		err := c.once(ctx, call, method, u.String(), body, out)
		var werr *Error
		switch {
		case err == nil:
			return nil
		case ctx.Err() != nil:
			return err
		case errors.As(err, &werr) && werr.handling() != retry:
			return err
		}
		switch {
		case werr != nil && werr.Code == SessionExists && attempt == 0:
			c.blocked.Store(true)
			// Expected after a crash: GitHub releases the old session in about a minute.
			c.log.Info("waiting for GitHub to release the previous session", "call", call, "err", err)
		case werr != nil && werr.Code == SessionExists:
		default:
			c.log.Warn("worker call failed, retrying", "call", call, "attempt", attempt+1, "err", err)
		}
		if werr := c.backoff.Wait(ctx, attempt); werr != nil {
			return err
		}
	}
}

func (c *Client) once(ctx context.Context, call, method, u string, body []byte, out any) error {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Boxrunner-Version", c.version)
	req.Header.Set("User-Agent", "boxrunner/"+c.version)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("worker %s: %w", call, err)
	}
	defer resp.Body.Close()
	version := resp.Header.Get("Boxrunner-Worker-Version")
	c.workerVersion.Store(&version)
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("worker %s: %w", call, err)
	}
	if resp.StatusCode >= 300 {
		e := &Error{Status: resp.StatusCode, Call: call}
		var shape struct {
			Error   Code   `json:"error"`
			Message string `json:"message"`
			HeldBy  string `json:"heldBy"`
		}
		if json.Unmarshal(data, &shape) == nil {
			e.Code, e.Message = shape.Error, shape.Message
			if e.Code == SessionExists {
				e.HeldBy = shape.HeldBy
			}
		}
		if e.Message == "" {
			e.Message = http.StatusText(resp.StatusCode)
		}
		return e
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return &Error{Status: resp.StatusCode, Call: call, Message: "unreadable answer: " + err.Error()}
	}
	return nil
}

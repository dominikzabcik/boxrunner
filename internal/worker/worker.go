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
	"sync/atomic"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/backoff"
	"github.com/dominikzabcik/boxrunner/internal/queue"
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
}

func (e *Error) Error() string {
	code := string(e.Code)
	if code == "" {
		code = "no error code"
	}
	return fmt.Sprintf("worker %s: %d %s: %s", e.Call, e.Status, code, e.Message)
}

func (e *Error) handling() handling {
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

// SessionBlocked reports whether a session an earlier owner left open ever
// refused a new one.
func (c *Client) SessionBlocked() bool { return c.blocked.Load() }

// Fatal reports whether err means the agent must stop: a wrong token, an
// agent too old for its Worker, or a Worker whose setup is unfinished.
func Fatal(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.handling() == fatal
}

type Repo struct {
	Name string `json:"name"`
	ID   int64  `json:"id"`
}

type Repos struct {
	Repos   []Repo   `json:"repos"`
	Classes []string `json:"classes"`
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
}

type HeartbeatDisk struct {
	Free  int64 `json:"free"`
	Floor int64 `json:"floor"`
}

type HeartbeatClass struct {
	Name     string `json:"name"`
	Priority string `json:"priority"`
	Max      int    `json:"max"`
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
}

// SendHeartbeat makes one attempt: the next heartbeat follows anyway.
func (c *Client) SendHeartbeat(ctx context.Context, hb Heartbeat) error {
	body, err := json.Marshal(hb)
	if err != nil {
		return err
	}
	return c.once(ctx, "heartbeat", http.MethodPost, c.base.JoinPath("/v1/heartbeat").String(), body, nil)
}

func (c *Client) Repos(ctx context.Context) (Repos, error) {
	var out Repos
	err := c.do(ctx, "list repositories", http.MethodGet, "/v1/repos", nil, nil, &out)
	return out, err
}

func (c *Client) ScaleSet(ctx context.Context, repo, class string) (ScaleSet, error) {
	var out ScaleSet
	err := c.do(ctx, "find scale set", http.MethodPost, "/v1/scalesets", nil,
		map[string]any{"repo": repo, "class": class}, &out)
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
// still holds the scale set it keeps retrying; GitHub releases an abandoned
// one after about a minute.
func (c *Client) OpenSession(ctx context.Context, repo string, scaleSetID int64, owner string) (Session, error) {
	var out sessionResponse
	err := c.do(ctx, "open session", http.MethodPost, "/v1/sessions", nil,
		map[string]any{"repo": repo, "scaleSetId": scaleSetID, "owner": owner}, &out)
	return out.session(), err
}

func (c *Client) RefreshSession(ctx context.Context, repo string, scaleSetID int64, sessionID string) (Session, error) {
	var out sessionResponse
	err := c.do(ctx, "refresh session", http.MethodPatch, "/v1/sessions/"+url.PathEscape(sessionID), nil,
		map[string]any{"repo": repo, "scaleSetId": scaleSetID}, &out)
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

func (c *Client) MintRunner(ctx context.Context, repo string, scaleSetID int64, name string) (Runner, error) {
	var out Runner
	err := c.do(ctx, "mint runner", http.MethodPost, "/v1/runners", nil,
		map[string]any{"repo": repo, "scaleSetId": scaleSetID, "name": name}, &out)
	return out, err
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
	return url.Values{"repo": {repo}, "scaleSetId": {strconv.FormatInt(scaleSetID, 10)}}
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
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("worker %s: %w", call, err)
	}
	if resp.StatusCode >= 300 {
		e := &Error{Status: resp.StatusCode, Call: call}
		var shape struct {
			Error   Code   `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &shape) == nil {
			e.Code, e.Message = shape.Error, shape.Message
		}
		if e.Message == "" {
			e.Message = http.StatusText(resp.StatusCode)
		}
		return e
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return &Error{Status: resp.StatusCode, Call: call, Message: "unreadable answer: " + err.Error()}
	}
	return nil
}

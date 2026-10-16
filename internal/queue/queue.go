// Package queue talks to a runner scale set's message queue at GitHub with a
// session token: get a message, acquire the jobs it offers, acknowledge it.
//
// Ported from github.com/actions/scaleset (session_client.go, types.go and the
// message parsing in client.go at commit e6daac7), MIT licensed, Copyright
// GitHub, Inc.; the license is in NOTICE next to this file. The port drops the
// admin-token client: here the Worker opens and refreshes sessions.
package queue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Session is what the agent needs to talk to one scale set's queue.
type Session struct {
	URL            string
	Token          string
	AcquireJobsURL string
}

type Statistics struct {
	TotalAvailableJobs     int `json:"totalAvailableJobs"`
	TotalAcquiredJobs      int `json:"totalAcquiredJobs"`
	TotalAssignedJobs      int `json:"totalAssignedJobs"`
	TotalRunningJobs       int `json:"totalRunningJobs"`
	TotalRegisteredRunners int `json:"totalRegisteredRunners"`
	TotalBusyRunners       int `json:"totalBusyRunners"`
	TotalIdleRunners       int `json:"totalIdleRunners"`
}

type Job struct {
	RequestID          int64     `json:"runnerRequestId"`
	Repository         string    `json:"repositoryName"`
	Owner              string    `json:"ownerName"`
	JobID              string    `json:"jobId"`
	WorkflowRef        string    `json:"jobWorkflowRef"`
	DisplayName        string    `json:"jobDisplayName"`
	WorkflowRunID      int64     `json:"workflowRunId"`
	Event              string    `json:"eventName"`
	Labels             []string  `json:"requestLabels"`
	QueueTime          time.Time `json:"queueTime"`
	ScaleSetAssignTime time.Time `json:"scaleSetAssignTime"`
	RunnerAssignTime   time.Time `json:"runnerAssignTime"`
	FinishTime         time.Time `json:"finishTime"`
}

// FullName is the job's repository as owner/name. A job offered to an
// organization scale set names its repository apart from its owner; owner
// stands in when the job leaves its owner out. It is "" when the job names no
// repository.
func (j Job) FullName(owner string) string {
	switch {
	case j.Repository == "" || strings.Contains(j.Repository, "/"):
		return j.Repository
	case j.Owner != "":
		owner = j.Owner
	}
	return owner + "/" + j.Repository
}

type JobStarted struct {
	Job
	RunnerID   int64  `json:"runnerId"`
	RunnerName string `json:"runnerName"`
}

type JobCompleted struct {
	Job
	Result     string `json:"result"`
	RunnerID   int64  `json:"runnerId"`
	RunnerName string `json:"runnerName"`
}

type Message struct {
	ID         int64
	Statistics *Statistics
	Available  []Job
	Assigned   []Job
	Started    []JobStarted
	Completed  []JobCompleted
}

// ErrTokenExpired means the session needs a refresh through the Worker.
var ErrTokenExpired = errors.New("queue token expired")

// StatusError is an answer the queue protocol does not expect.
type StatusError struct {
	Call   string
	Status int
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s: unexpected status %d: %s", e.Call, e.Status, e.Body)
}

type Client struct {
	HTTP      *http.Client
	UserAgent string
}

// Get long-polls for the next message. It returns nil when none arrived.
func (c *Client) Get(ctx context.Context, s Session, lastID int64, capacity int) (*Message, error) {
	u, err := url.Parse(s.URL)
	if err != nil {
		return nil, fmt.Errorf("queue url: %w", err)
	}
	if lastID > 0 {
		q := u.Query()
		q.Set("lastMessageId", strconv.FormatInt(lastID, 10))
		u.RawQuery = q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json; api-version=6.0-preview")
	req.Header.Set("X-ScaleSetMaxCapacity", strconv.Itoa(capacity))
	resp, err := c.do(req, s.Token)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusAccepted:
		return nil, nil
	case http.StatusOK:
		return parseMessage(resp.Body)
	default:
		return nil, statusError("get message", resp)
	}
}

// Ack deletes a handled message from the queue.
func (c *Client) Ack(ctx context.Context, s Session, id int64) error {
	u, err := url.Parse(s.URL)
	if err != nil {
		return fmt.Errorf("queue url: %w", err)
	}
	u.Path = fmt.Sprintf("%s/%d", u.Path, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req, s.Token)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return statusError("ack message", resp)
	}
	return nil
}

// Acquire claims offered jobs for this scale set and returns the ids GitHub
// granted. Acquiring a job twice is harmless.
func (c *Client) Acquire(ctx context.Context, s Session, requestIDs []int64) ([]int64, error) {
	body, err := json.Marshal(requestIDs)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.AcquireJobsURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req, s.Token)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusError("acquire jobs", resp)
	}
	var out struct {
		Value []int64 `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("acquire jobs: %w", err)
	}
	return out.Value, nil
}

func (c *Client) do(req *http.Request, token string) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+token)
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		return nil, ErrTokenExpired
	}
	return resp, nil
}

func statusError(call string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return &StatusError{Call: call, Status: resp.StatusCode, Body: string(bytes.TrimSpace(body))}
}

type messageResponse struct {
	MessageID   int64       `json:"messageId"`
	MessageType string      `json:"messageType"`
	Body        string      `json:"body"`
	Statistics  *Statistics `json:"statistics"`
}

func parseMessage(r io.Reader) (*Message, error) {
	var resp messageResponse
	if err := json.NewDecoder(r).Decode(&resp); err != nil {
		return nil, fmt.Errorf("message: %w", err)
	}
	if resp.MessageType != "RunnerScaleSetJobMessages" {
		return nil, fmt.Errorf("message: unsupported type %q", resp.MessageType)
	}
	msg := &Message{ID: resp.MessageID, Statistics: resp.Statistics}
	if resp.Body == "" {
		return msg, nil
	}
	var batch []json.RawMessage
	if err := json.Unmarshal([]byte(resp.Body), &batch); err != nil {
		return nil, fmt.Errorf("message body: %w", err)
	}
	for _, raw := range batch {
		var kind struct {
			MessageType string `json:"messageType"`
		}
		if err := json.Unmarshal(raw, &kind); err != nil {
			return nil, fmt.Errorf("job message: %w", err)
		}
		var err error
		switch kind.MessageType {
		case "JobAvailable":
			err = appendDecoded(raw, &msg.Available)
		case "JobAssigned":
			err = appendDecoded(raw, &msg.Assigned)
		case "JobStarted":
			err = appendDecoded(raw, &msg.Started)
		case "JobCompleted":
			err = appendDecoded(raw, &msg.Completed)
		default:
			// The service may add kinds; ignoring one is safer than refusing the batch.
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", kind.MessageType, err)
		}
	}
	return msg, nil
}

func appendDecoded[T any](raw json.RawMessage, list *[]T) error {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	*list = append(*list, v)
	return nil
}

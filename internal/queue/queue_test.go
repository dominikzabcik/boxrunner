package queue

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

type recorded struct {
	method, path, query, auth, accept, capacity, body string
}

func server(t *testing.T, status int, body string) (*httptest.Server, *[]recorded) {
	var seen []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = append(seen, recorded{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("Accept"), r.Header.Get("X-ScaleSetMaxCapacity"), string(b)})
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func session(srv *httptest.Server) Session {
	return Session{URL: srv.URL + "/queue/7", Token: "tok", AcquireJobsURL: srv.URL + "/_apis/runtime/runnerscalesets/7/acquirejobs?api-version=6.0-preview"}
}

const messageBody = `{
  "messageId": 12,
  "messageType": "RunnerScaleSetJobMessages",
  "statistics": {"totalAvailableJobs": 1, "totalAssignedJobs": 2, "totalRunningJobs": 1},
  "body": "[{\"messageType\":\"JobAvailable\",\"runnerRequestId\":501,\"repositoryName\":\"site\",\"ownerName\":\"octo\",\"jobDisplayName\":\"test\",\"queueTime\":\"2026-10-13T21:26:18Z\",\"acquireJobUrl\":\"x\"},{\"messageType\":\"JobStarted\",\"runnerRequestId\":500,\"runnerId\":41,\"runnerName\":\"box-1-boxrunner-3f9a2c\"},{\"messageType\":\"JobCompleted\",\"runnerRequestId\":499,\"result\":\"succeeded\",\"runnerId\":40,\"runnerName\":\"box-1-boxrunner-000001\"},{\"messageType\":\"JobSomethingNew\"}]"
}`

func TestGetMessage(t *testing.T) {
	srv, seen := server(t, 200, messageBody)
	c := &Client{}
	got, err := c.Get(context.Background(), session(srv), 11, 2)
	if err != nil {
		t.Fatal(err)
	}
	queued := time.Date(2026, 10, 13, 21, 26, 18, 0, time.UTC)
	want := &Message{
		ID:         12,
		Statistics: &Statistics{TotalAvailableJobs: 1, TotalAssignedJobs: 2, TotalRunningJobs: 1},
		Available:  []Job{{RequestID: 501, Repository: "site", Owner: "octo", DisplayName: "test", QueueTime: queued}},
		Started:    []JobStarted{{Job: Job{RequestID: 500}, RunnerID: 41, RunnerName: "box-1-boxrunner-3f9a2c"}},
		Completed:  []JobCompleted{{Job: Job{RequestID: 499}, Result: "succeeded", RunnerID: 40, RunnerName: "box-1-boxrunner-000001"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Get =\n%+v\nwant\n%+v", got, want)
	}
	wantReq := []recorded{{"GET", "/queue/7", "lastMessageId=11", "Bearer tok", "application/json; api-version=6.0-preview", "2", ""}}
	if !reflect.DeepEqual(*seen, wantReq) {
		t.Errorf("requests = %+v", *seen)
	}
}

func TestGetNoMessage(t *testing.T) {
	srv, seen := server(t, 202, "")
	got, err := (&Client{}).Get(context.Background(), session(srv), 0, 1)
	if got != nil || err != nil {
		t.Errorf("Get = %v, %v; want nil, nil", got, err)
	}
	if (*seen)[0].query != "" {
		t.Errorf("first poll sent lastMessageId: %q", (*seen)[0].query)
	}
}

func TestExpiredToken(t *testing.T) {
	srv, _ := server(t, 401, "")
	c := &Client{}
	s := session(srv)
	if _, err := c.Get(context.Background(), s, 0, 1); !errors.Is(err, ErrTokenExpired) {
		t.Errorf("Get error = %v", err)
	}
	if err := c.Ack(context.Background(), s, 3); !errors.Is(err, ErrTokenExpired) {
		t.Errorf("Ack error = %v", err)
	}
	if _, err := c.Acquire(context.Background(), s, []int64{1}); !errors.Is(err, ErrTokenExpired) {
		t.Errorf("Acquire error = %v", err)
	}
}

func TestAck(t *testing.T) {
	srv, seen := server(t, 204, "")
	if err := (&Client{}).Ack(context.Background(), session(srv), 12); err != nil {
		t.Fatal(err)
	}
	if r := (*seen)[0]; r.method != "DELETE" || r.path != "/queue/7/12" || r.auth != "Bearer tok" {
		t.Errorf("ack request = %+v", r)
	}
}

func TestAcquire(t *testing.T) {
	srv, seen := server(t, 200, `{"count":1,"value":[501]}`)
	got, err := (&Client{}).Acquire(context.Background(), session(srv), []int64{501, 502})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []int64{501}) {
		t.Errorf("acquired %v", got)
	}
	want := recorded{"POST", "/_apis/runtime/runnerscalesets/7/acquirejobs", "api-version=6.0-preview", "Bearer tok", "", "", "[501,502]"}
	if (*seen)[0] != want {
		t.Errorf("acquire request = %+v", (*seen)[0])
	}
}

func TestUnexpectedStatus(t *testing.T) {
	srv, _ := server(t, 500, "boom\n")
	_, err := (&Client{}).Get(context.Background(), session(srv), 0, 1)
	if err == nil || err.Error() != "get message: unexpected status 500: boom" {
		t.Errorf("error = %v", err)
	}
}

func TestJobFullName(t *testing.T) {
	cases := []struct {
		job  Job
		want string
	}{
		{Job{Repository: "site", Owner: "octo"}, "octo/site"},
		{Job{Repository: "octo/site"}, "octo/site"},
		{Job{Repository: "site"}, "fallback/site"},
		{Job{Owner: "octo"}, ""},
	}
	for _, c := range cases {
		if got := c.job.FullName("fallback"); got != c.want {
			t.Errorf("%+v.FullName = %q, want %q", c.job, got, c.want)
		}
	}
}

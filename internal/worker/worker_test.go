package worker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/backoff"
	"github.com/dominikzabcik/boxrunner/internal/queue"
)

type reply struct {
	status int
	body   string
}

type request struct {
	method, uri, auth, version, body string
}

// fake answers with replies in order and records each request.
func fake(t *testing.T, replies ...reply) (*Client, *[]request, *[]time.Duration) {
	var seen []request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = append(seen, request{r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"), r.Header.Get("Boxrunner-Version"), string(b)})
		next := replies[min(len(seen), len(replies))-1]
		w.Header().Set("Boxrunner-Protocol", "1")
		w.WriteHeader(next.status)
		io.WriteString(w, next.body)
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	var slept []time.Duration
	c := New(Options{
		URL: u, Token: "box-token", Version: "0.1.0",
		Backoff: backoff.Policy{
			Jitter: func() float64 { return 0 },
			Sleep: func(_ context.Context, d time.Duration) error {
				slept = append(slept, d)
				return nil
			},
		},
	})
	return c, &seen, &slept
}

func TestRepos(t *testing.T) {
	c, seen, _ := fake(t, reply{200, `{"repos":[{"name":"octo/site","id":123}],"classes":["boxrunner","boxrunner-light"]}`})
	got, err := c.Repos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := Repos{Repos: []Repo{{Name: "octo/site", ID: 123}}, Classes: []string{"boxrunner", "boxrunner-light"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Repos = %+v", got)
	}
	if r := (*seen)[0]; r != (request{"GET", "/v1/repos", "Bearer box-token", "0.1.0", ""}) {
		t.Errorf("request = %+v", r)
	}
}

func TestRequestShapes(t *testing.T) {
	session := `{"sessionId":"5d6e","queueUrl":"https://q.example/7","queueToken":"eyJ","acquireJobsUrl":"https://q.example/acquire","statistics":{"totalAssignedJobs":1}}`
	c, seen, _ := fake(t,
		reply{200, `{"id":7,"name":"boxrunner"}`},
		reply{200, session},
		reply{200, session},
		reply{204, ""},
		reply{200, `{"runnerId":41,"jitConfig":"eyIu"}`},
		reply{204, ""},
	)
	ctx := context.Background()
	ss, err := c.ScaleSet(ctx, "octo/site", "boxrunner")
	if err != nil || ss != (ScaleSet{ID: 7, Name: "boxrunner"}) {
		t.Errorf("ScaleSet = %+v, %v", ss, err)
	}
	s, err := c.OpenSession(ctx, "octo/site", 7, "box-1")
	wantSession := Session{
		ID:         "5d6e",
		Queue:      queue.Session{URL: "https://q.example/7", Token: "eyJ", AcquireJobsURL: "https://q.example/acquire"},
		Statistics: queue.Statistics{TotalAssignedJobs: 1},
	}
	if err != nil || s != wantSession {
		t.Errorf("OpenSession = %+v, %v", s, err)
	}
	if s, err := c.RefreshSession(ctx, "octo/site", 7, "5d6e"); err != nil || s != wantSession {
		t.Errorf("RefreshSession = %+v, %v", s, err)
	}
	if err := c.CloseSession(ctx, "octo/site", 7, "5d6e"); err != nil {
		t.Error(err)
	}
	r, err := c.MintRunner(ctx, "octo/site", 7, "box-1-boxrunner-3f9a2c")
	if err != nil || r != (Runner{ID: 41, JITConfig: "eyIu"}) {
		t.Errorf("MintRunner = %+v, %v", r, err)
	}
	if err := c.RemoveRunner(ctx, "octo/site", 7, 41); err != nil {
		t.Error(err)
	}
	want := []request{
		{"POST", "/v1/scalesets", "Bearer box-token", "0.1.0", `{"class":"boxrunner","repo":"octo/site"}`},
		{"POST", "/v1/sessions", "Bearer box-token", "0.1.0", `{"owner":"box-1","repo":"octo/site","scaleSetId":7}`},
		{"PATCH", "/v1/sessions/5d6e", "Bearer box-token", "0.1.0", `{"repo":"octo/site","scaleSetId":7}`},
		{"DELETE", "/v1/sessions/5d6e?repo=octo%2Fsite&scaleSetId=7", "Bearer box-token", "0.1.0", ""},
		{"POST", "/v1/runners", "Bearer box-token", "0.1.0", `{"name":"box-1-boxrunner-3f9a2c","repo":"octo/site","scaleSetId":7}`},
		{"DELETE", "/v1/runners/41?repo=octo%2Fsite&scaleSetId=7", "Bearer box-token", "0.1.0", ""},
	}
	if !reflect.DeepEqual(*seen, want) {
		t.Errorf("requests =\n%+v\nwant\n%+v", *seen, want)
	}
}

func errorBody(code string) string {
	return fmt.Sprintf(`{"error":%q,"message":"said the Worker"}`, code)
}

// Every error code from docs/api.md: either the client retries it (and the
// second answer succeeds) or the caller gets it back after one request.
func TestErrorCodes(t *testing.T) {
	cases := []struct {
		status   int
		code     Code
		retried  bool
		fatal    bool
		requests int
	}{
		{400, BadRequest, false, false, 1},
		{400, UnknownClass, false, false, 1},
		{401, Unauthorized, false, true, 1},
		{404, NotFound, false, false, 1},
		{409, SessionExists, true, false, 2},
		{409, RunnerBusy, false, false, 1},
		{410, RepoIneligible, false, false, 1},
		{426, UpgradeRequired, false, true, 1},
		{429, RateLimited, true, false, 2},
		{502, GitHubError, true, false, 2},
		{503, NotConfigured, false, true, 1},
	}
	for _, c := range cases {
		t.Run(string(c.code), func(t *testing.T) {
			client, seen, slept := fake(t, reply{c.status, errorBody(string(c.code))}, reply{200, `{"repos":[],"classes":[]}`})
			_, err := client.Repos(context.Background())
			if len(*seen) != c.requests {
				t.Errorf("%d requests; want %d", len(*seen), c.requests)
			}
			if blocked := client.SessionBlocked(); blocked != (c.code == SessionExists) {
				t.Errorf("SessionBlocked = %v", blocked)
			}
			if c.retried {
				if err != nil || !reflect.DeepEqual(*slept, []time.Duration{500 * time.Millisecond}) {
					t.Errorf("err = %v, slept %v", err, *slept)
				}
				return
			}
			want := fmt.Sprintf("worker list repositories: %d %s: said the Worker", c.status, c.code)
			if err == nil || err.Error() != want || !Is(err, c.code) || Fatal(err) != c.fatal {
				t.Errorf("err = %v (fatal %v); want %s (fatal %v)", err, Fatal(err), want, c.fatal)
			}
		})
	}
}

func TestNotFoundMeansGone(t *testing.T) {
	c, _, _ := fake(t, reply{404, errorBody("not_found")})
	if err := c.RemoveRunner(context.Background(), "octo/site", 7, 41); err != nil {
		t.Errorf("RemoveRunner = %v; want nil", err)
	}
	if err := c.CloseSession(context.Background(), "octo/site", 7, "5d6e"); err != nil {
		t.Errorf("CloseSession = %v; want nil", err)
	}
}

func TestBackoffGrowsAndRetriesBareFailures(t *testing.T) {
	c, seen, slept := fake(t,
		reply{502, "<html>bad gateway</html>"},
		reply{503, ""},
		reply{429, errorBody("rate_limited")},
		reply{500, errorBody("github_error")},
		reply{200, `{"repos":[],"classes":["boxrunner"]}`},
	)
	got, err := c.Repos(context.Background())
	if err != nil || !reflect.DeepEqual(got.Classes, []string{"boxrunner"}) {
		t.Fatalf("Repos = %+v, %v", got, err)
	}
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second}
	if len(*seen) != 5 || !reflect.DeepEqual(*slept, want) {
		t.Errorf("%d requests, slept %v; want 5 and %v", len(*seen), *slept, want)
	}
}

func TestRetryStopsWithContext(t *testing.T) {
	c, seen, _ := fake(t, reply{502, errorBody("github_error")})
	ctx, cancel := context.WithCancel(context.Background())
	c.backoff.Sleep = func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}
	_, err := c.Repos(ctx)
	if !Is(err, GitHubError) || len(*seen) != 1 {
		t.Errorf("err = %v after %d requests", err, len(*seen))
	}
}

func TestUnreadableAnswerIsNotRetried(t *testing.T) {
	c, seen, _ := fake(t, reply{200, `not json`})
	_, err := c.Repos(context.Background())
	want := "worker list repositories: 200 no error code: unreadable answer: invalid character 'o' in literal null (expecting 'u')"
	if err == nil || err.Error() != want || len(*seen) != 1 {
		t.Errorf("err = %v after %d requests", err, len(*seen))
	}
}

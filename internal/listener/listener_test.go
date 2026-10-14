package listener

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/queue"
	"github.com/dominikzabcik/boxrunner/internal/scheduler"
	"github.com/dominikzabcik/boxrunner/internal/worker"
)

var t0 = time.Date(2026, 10, 13, 21, 0, 0, 0, time.UTC)

var key = scheduler.Key{Repo: "octo/site", Class: "boxrunner"}

type fakeWorker struct {
	mu         sync.Mutex
	calls      []string
	queueURL   string
	refreshErr error
}

func (w *fakeWorker) session(id, token string, assigned int) worker.Session {
	return worker.Session{
		ID:         id,
		Queue:      queue.Session{URL: w.queueURL + "/queue/7", Token: token, AcquireJobsURL: w.queueURL + "/acquire"},
		Statistics: queue.Statistics{TotalAssignedJobs: assigned},
	}
}

func (w *fakeWorker) record(s string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, s)
}

func (w *fakeWorker) OpenSession(_ context.Context, repo string, id int64, owner string) (worker.Session, error) {
	w.record(fmt.Sprintf("open %s %d %s", repo, id, owner))
	return w.session("s1", "t1", 1), nil
}

func (w *fakeWorker) RefreshSession(_ context.Context, repo string, id int64, session string) (worker.Session, error) {
	w.record(fmt.Sprintf("refresh %s %d %s", repo, id, session))
	if w.refreshErr != nil {
		return worker.Session{}, w.refreshErr
	}
	return w.session("s1", "t2", 1), nil
}

func (w *fakeWorker) CloseSession(_ context.Context, repo string, id int64, session string) error {
	w.record(fmt.Sprintf("close %s %d %s", repo, id, session))
	return nil
}

const jobMessage = `{"messageId":5,"messageType":"RunnerScaleSetJobMessages","statistics":{"totalAssignedJobs":2},
"body":"[{\"messageType\":\"JobAvailable\",\"runnerRequestId\":501,\"queueTime\":\"2026-10-13T21:01:00Z\"}]"}`

// queueServer plays GitHub's queue: a 401 first, then one message, then long
// polls that hold until the test ends.
func queueServer(t *testing.T) (*httptest.Server, func() []string) {
	var mu sync.Mutex
	var seen []string
	gets := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, fmt.Sprintf("%s %s %s %s cap=%s", r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"), body, r.Header.Get("X-ScaleSetMaxCapacity")))
		if r.Method == http.MethodGet {
			gets++
		}
		n := gets
		mu.Unlock()
		switch {
		case r.Method == http.MethodGet && n == 1 && r.Header.Get("Authorization") == "Bearer t1":
			w.WriteHeader(http.StatusUnauthorized)
		case r.Method == http.MethodGet && r.URL.Query().Get("lastMessageId") == "":
			io.WriteString(w, jobMessage)
		case r.Method == http.MethodGet:
			<-r.Context().Done()
		case r.Method == http.MethodPost:
			io.WriteString(w, `{"count":1,"value":[501]}`)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func start(t *testing.T, w *fakeWorker, now func() time.Time) (chan scheduler.Event, context.CancelFunc, chan error) {
	events := make(chan scheduler.Event)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{
			ScaleSet: key, ScaleSetID: 7, Owner: "box-1", Capacity: 2,
			Worker: w, Queue: &queue.Client{}, Events: events, Now: now,
		})
	}()
	t.Cleanup(cancel)
	return events, cancel, done
}

func next(t *testing.T, events chan scheduler.Event) scheduler.Event {
	select {
	case e := <-events:
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
		return nil
	}
}

func TestListenRefreshAcquireAck(t *testing.T) {
	srv, seen := queueServer(t)
	w := &fakeWorker{queueURL: srv.URL}
	events, cancel, done := start(t, w, func() time.Time { return t0 })

	first := next(t, events)
	second := next(t, events)
	for deadline := time.Now().Add(5 * time.Second); len(seen()) < 5 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}

	// The session opened with one assigned job the listener had never seen.
	if want := (scheduler.Update{ScaleSet: key, ID: 7, Jobs: []time.Time{t0}}); !reflect.DeepEqual(first, want) {
		t.Errorf("first event = %#v", first)
	}
	if want := (scheduler.Update{ScaleSet: key, ID: 7, Jobs: []time.Time{t0, t0.Add(time.Minute)}}); !reflect.DeepEqual(second, want) {
		t.Errorf("second event = %#v", second)
	}
	wantCalls := []string{"open octo/site 7 box-1", "refresh octo/site 7 s1", "close octo/site 7 s1"}
	if !reflect.DeepEqual(w.calls, wantCalls) {
		t.Errorf("worker calls = %q", w.calls)
	}
	wantQueue := []string{
		"GET /queue/7 Bearer t1  cap=2",
		"GET /queue/7 Bearer t2  cap=2",
		"POST /acquire Bearer t2 [501] cap=",
		"DELETE /queue/7/5 Bearer t2  cap=",
		"GET /queue/7?lastMessageId=5 Bearer t2  cap=2",
	}
	if got := seen(); !reflect.DeepEqual(got, wantQueue) {
		t.Errorf("queue requests =\n%q\nwant\n%q", got, wantQueue)
	}
}

func TestRefreshBeforeExpiry(t *testing.T) {
	srv, seen := queueServer(t)
	w := &fakeWorker{queueURL: srv.URL}
	calls := 0
	events, cancel, done := start(t, w, func() time.Time {
		calls++
		if calls <= 2 {
			return t0
		}
		return t0.Add(61 * time.Minute)
	})
	next(t, events)
	next(t, events)
	cancel()
	<-done
	if got := seen()[0]; got != "GET /queue/7 Bearer t2  cap=2" {
		t.Errorf("first poll = %q; want it after a refresh", got)
	}
	if !reflect.DeepEqual(w.calls[:2], []string{"open octo/site 7 box-1", "refresh octo/site 7 s1"}) {
		t.Errorf("worker calls = %q", w.calls)
	}
}

func TestIneligibleRepoStops(t *testing.T) {
	srv, _ := queueServer(t)
	gone := &worker.Error{Status: 410, Code: worker.RepoIneligible, Message: "public", Call: "refresh session"}
	w := &fakeWorker{queueURL: srv.URL, refreshErr: gone}
	events, _, done := start(t, w, func() time.Time { return t0 })
	next(t, events)
	if err := <-done; err != gone {
		t.Errorf("Run = %v; want %v", err, gone)
	}
	if want := []string{"open octo/site 7 box-1", "refresh octo/site 7 s1", "close octo/site 7 s1"}; !reflect.DeepEqual(w.calls, want) {
		t.Errorf("worker calls = %q", w.calls)
	}
}

func TestCompletedJobKeepsItsQueueTime(t *testing.T) {
	events := make(chan scheduler.Event, 2)
	l := &listener{Config: Config{ScaleSet: key, ScaleSetID: 7, Events: events, Now: func() time.Time { return t0 }}, jobs: map[int64]time.Time{}}
	queued := t0.Add(-8 * time.Second)
	ctx := context.Background()
	if err := l.publish(ctx, &queue.Message{Available: []queue.Job{{RequestID: 501, QueueTime: queued}}}, &queue.Statistics{TotalAssignedJobs: 1}); err != nil {
		t.Fatal(err)
	}
	<-events
	if err := l.publish(ctx, &queue.Message{Completed: []queue.JobCompleted{{Job: queue.Job{RequestID: 501}, Result: "succeeded"}}}, &queue.Statistics{}); err != nil {
		t.Fatal(err)
	}
	u := (<-events).(scheduler.Update)
	if got := u.Completed[0].QueueTime; !got.Equal(queued) {
		t.Errorf("completed job queue time = %v, want %v", got, queued)
	}
}

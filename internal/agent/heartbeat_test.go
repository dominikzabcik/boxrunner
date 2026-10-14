package agent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/status"
)

func TestHeartbeatBody(t *testing.T) {
	t0 := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	snap := status.Snapshot{
		Box: "box-1", Version: "0.2.0", Latest: "0.2.2", Image: status.ImageStatus{Built: "2.338.0+3f9a2c0b1d4e"},
		DiskFree: 26e9, DiskFloor: 10e9,
		Classes: []status.Class{{Name: "boxrunner", Priority: "normal", Max: 1, Active: 1}},
		Runners: []status.Runner{{Name: "box-1-boxrunner-3f9a2c", Repo: "octo/site", Class: "boxrunner", Phase: "running", Since: t0}},
	}
	jobs := []status.Job{{Runner: "box-1-boxrunner-000001", Repo: "octo/site", Class: "boxrunner", Name: "test", Result: "canceled", Finished: t0}}
	body, _ := json.Marshal(heartbeatBody(snap, jobs))
	want := `{"box":"box-1","version":"0.2.0","image":"2.338.0+3f9a2c0b1d4e","disk":{"free":26000000000,"floor":10000000000},` +
		`"classes":[{"name":"boxrunner","priority":"normal","max":1}],` +
		`"runners":[{"name":"box-1-boxrunner-3f9a2c","repo":"octo/site","class":"boxrunner","state":"running","since":"2026-10-14T09:00:00Z"}],` +
		`"jobs":[{"runner":"box-1-boxrunner-000001","repo":"octo/site","class":"boxrunner","name":"test","result":"canceled","queued":null,"started":null,"finished":"2026-10-14T09:00:00Z"}],"latest":"0.2.2"}`
	if string(body) != want {
		t.Errorf("body =\n%s\nwant\n%s", body, want)
	}
}

func TestHeartbeatBodyTellsWhyJobsMayWait(t *testing.T) {
	held := time.Date(2026, 10, 14, 9, 5, 0, 0, time.UTC)
	snap := status.Snapshot{
		Box: "box-1", Version: "0.5.0", Image: status.ImageStatus{Built: "2.338.0+3f9a2c0b1d4e"}, Draining: true,
		Classes: []status.Class{
			{Name: "boxrunner", Priority: "normal", Max: 1},
			{Name: "boxrunner-light", Priority: "idle", Max: 2, HeldUntil: held},
		},
		ScaleSets: []status.ScaleSet{
			{Repo: "octo/docs", Class: "boxrunner", Assigned: 0},
			{Repo: "octo/site", Class: "boxrunner-light", Assigned: 3},
		},
	}
	body, _ := json.Marshal(heartbeatBody(snap, nil))
	want := `{"box":"box-1","version":"0.5.0","image":"2.338.0+3f9a2c0b1d4e","disk":{"free":0,"floor":0},` +
		`"classes":[{"name":"boxrunner","priority":"normal","max":1},{"name":"boxrunner-light","priority":"idle","max":2,"heldUntil":"2026-10-14T09:05:00Z"}],` +
		`"runners":[],"jobs":[],"draining":true,"scaleSets":[{"repo":"octo/site","class":"boxrunner-light","assigned":3},{"repo":"octo/docs","class":"boxrunner","assigned":0}]}`
	if string(body) != want {
		t.Errorf("body =\n%s\nwant\n%s", body, want)
	}
}

func TestFitHeartbeatDropsOldestJobs(t *testing.T) {
	var jobs []status.Job
	for i := range 50 {
		jobs = append(jobs, status.Job{Runner: "r" + string(rune('a'+i%26)), Repo: "octo/site", Class: "boxrunner", Name: strings.Repeat("x", 190) + string(rune('A'+i%26)), Result: "succeeded"})
	}
	hb := fitHeartbeat(heartbeatBody(status.Snapshot{Box: "box-1"}, jobs), 4096)
	body, _ := json.Marshal(hb)
	if len(body) > 4096 || len(hb.Jobs) == 0 {
		t.Fatalf("body %d bytes with %d jobs", len(body), len(hb.Jobs))
	}
	if last := hb.Jobs[len(hb.Jobs)-1].Name; last != jobs[49].Name {
		t.Errorf("the newest job was dropped; last kept is %q", last[190:])
	}
}

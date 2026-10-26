package release

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseLatest(t *testing.T) {
	got, err := ParseLatest("https://github.com/actions/runner/releases/tag/v2.329.0")
	if err != nil || got != "2.329.0" {
		t.Errorf("ParseLatest = %q, %v", got, err)
	}
	if _, err := ParseLatest("https://github.com/login"); err == nil || err.Error() != `latest release: unexpected redirect "https://github.com/login"` {
		t.Errorf("ParseLatest of a login page = %v", err)
	}
}

func TestLatestFollowsNoRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("method %s", r.Method)
		}
		http.Redirect(w, r, "/dominikzabcik/boxrunner/releases/tag/v0.2.2", http.StatusFound)
	}))
	defer srv.Close()
	got, err := Latest(context.Background(), srv.Client(), srv.URL+"/dominikzabcik/boxrunner/releases/latest")
	if err != nil || got != "0.2.2" {
		t.Errorf("Latest = %q, %v", got, err)
	}
}

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.2.1", "0.2.1", 0},
		{"0.2.2", "0.2.1", 1},
		{"0.10.0", "0.9.9", 1},
		{"1.0.0", "0.99.0", 1},
		{"v0.2.1", "0.2.1", 0},
		{"0.3.0-rc.1", "0.3.0", -1},
		{"0.3.0-rc.2", "0.3.0-rc.1", 1},
		{"0.1.0-dev", "0.2.1", -1},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	if Newer("0.2.2", "") || Newer("", "0.2.2") || !Newer("0.2.2", "0.2.1") {
		t.Error("Newer treats an empty version as known")
	}
}

func TestMajor(t *testing.T) {
	for v, want := range map[string]int{"0.3.1": 0, "v1.0.0": 1, "12.4.0-rc.1": 12} {
		if got := Major(v); got != want {
			t.Errorf("Major(%q) = %d, want %d", v, got, want)
		}
	}
}

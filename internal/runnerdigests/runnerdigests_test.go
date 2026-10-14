package runnerdigests

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

const (
	digest338 = "sha256:4ffadc0002b2581327e06101fc8c06cd189232baf79fe561fac9caeb76f5e807"
	digest337 = "sha256:e5496277be5d09bc968b3d64911b74e219ac4a3f2edce956a3ecf9271bea1ef4"
)

// fake serves the runner's latest-release page, the list and ghcr.io's token
// and manifest endpoints: tags maps a tag to the digest the registry names.
func fake(t *testing.T, latest, list string, tags map[string]string) Source {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/releases/latest":
			http.Redirect(w, r, "/releases/tag/v"+latest, http.StatusFound)
		case r.URL.Path == "/runner-digests.json" && list != "":
			w.Write([]byte(list))
		case r.URL.Path == "/token" && r.URL.Query().Get("scope") == "repository:actions/actions-runner:pull":
			w.Write([]byte(`{"token":"anonymous"}`))
		case r.Method == http.MethodHead && strings.HasPrefix(r.URL.Path, "/v2/actions/actions-runner/manifests/") && r.Header.Get("Authorization") == "Bearer anonymous":
			digest, ok := tags[strings.TrimPrefix(r.URL.Path, "/v2/actions/actions-runner/manifests/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Docker-Content-Digest", digest)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return Source{Releases: srv.URL + "/releases/latest", List: srv.URL + "/runner-digests.json", Registry: srv.URL}
}

func TestChoose(t *testing.T) {
	list := List{"2.337.0": digest337, "2.338.0": digest338}
	cases := []struct {
		latest string
		want   Choice
	}{
		{"2.338.0", Choice{Version: "2.338.0", Digest: digest338}},
		{"2.339.0", Choice{Version: "2.338.0", Digest: digest338, Unlisted: "2.339.0"}},
		{"2.337.1", Choice{Version: "2.337.0", Digest: digest337, Unlisted: "2.337.1"}},
	}
	for _, c := range cases {
		got, err := list.Choose(c.latest)
		if err != nil || got != c.want {
			t.Errorf("Choose(%s) = %+v, %v; want %+v", c.latest, got, err, c.want)
		}
	}
	if got, err := list.Choose("2.336.0"); err == nil || err.Error() != "boxrunner's list of verified runner images has no release up to 2.336.0" {
		t.Errorf("Choose(2.336.0) = %+v, %v", got, err)
	}
}

func TestSourceChoose(t *testing.T) {
	src := fake(t, "2.339.0", `{"2.337.0":"`+digest337+`","2.338.0":"`+digest338+`"}`, nil)
	got, err := src.Choose(context.Background())
	if want := (Choice{Version: "2.338.0", Digest: digest338, Unlisted: "2.339.0"}); err != nil || got != want {
		t.Errorf("Choose = %+v, %v; want %+v", got, err, want)
	}
}

func TestFetch(t *testing.T) {
	src := fake(t, "2.338.0", `{"2.337.0":"`+digest337+`","2.338.0":"`+digest338+`"}`, nil)
	got, err := src.Fetch(context.Background())
	if want := (List{"2.337.0": digest337, "2.338.0": digest338}); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("Fetch = %v, %v; want %v", got, err, want)
	}
}

func TestFetchRefuses(t *testing.T) {
	cases := []struct{ name, list, want string }{
		{"a digest that is not sha256", `{"2.338.0":"md5:4ffa"}`, `has "2.338.0": "md5:4ffa", not a version and a sha256 digest`},
		{"a tag that is not a version", `{"latest":"` + digest338 + `"}`, `has "latest": "` + digest338 + `", not a version and a sha256 digest`},
		{"no list", "", "/runner-digests.json answered 404 Not Found"},
	}
	for _, c := range cases {
		src := fake(t, "2.338.0", c.list, nil)
		if got, err := src.Fetch(context.Background()); err == nil || !strings.HasSuffix(err.Error(), c.want) {
			t.Errorf("%s: Fetch = %v, %v; want an error ending %q", c.name, got, err, c.want)
		}
	}
}

func TestConfirm(t *testing.T) {
	src := fake(t, "2.338.0", "", map[string]string{"2.338.0": digest338, "2.337.0": digest337})
	if err := src.Confirm(context.Background(), "2.338.0", digest338); err != nil {
		t.Errorf("Confirm(2.338.0, its digest) = %v", err)
	}
	err := src.Confirm(context.Background(), "2.337.0", digest338)
	var mismatch *Mismatch
	if !errors.As(err, &mismatch) || err.Error() != "ghcr.io/actions/actions-runner:2.337.0 points to "+digest337+", not to "+digest338+" that boxrunner's CI verified" {
		t.Errorf("Confirm(2.337.0, 2.338.0's digest) = %v", err)
	}
	// A registry that does not know the tag says nothing about the image.
	err = src.Confirm(context.Background(), "2.339.0", digest338)
	if err == nil || errors.As(err, &mismatch) || !strings.HasSuffix(err.Error(), "/v2/actions/actions-runner/manifests/2.339.0 answered 404 Not Found") {
		t.Errorf("Confirm(2.339.0) = %v; want a plain error", err)
	}
}

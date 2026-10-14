package provenance

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/root"
)

// testdata holds what GitHub served for runner 2.338.0 on October 14, 2026:
// the attestation API's answer for its digest, and Sigstore's trusted root as
// sigstore-go's TUF client fetched it.
const (
	digest338 = "sha256:4ffadc0002b2581327e06101fc8c06cd189232baf79fe561fac9caeb76f5e807"
	digest337 = "sha256:e5496277be5d09bc968b3d64911b74e219ac4a3f2edce956a3ecf9271bea1ef4"
)

// fake serves ghcr.io's token and manifest endpoints and GitHub's attestation
// API: tags maps a tag to its digest, and attestations a digest to the API's
// answer.
func fake(t *testing.T, tags, attestations map[string]string) Checker {
	t.Helper()
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
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
			http.Error(w, "unexpected "+r.Method+" "+r.URL.String(), http.StatusTeapot)
		}
	}))
	t.Cleanup(registry.Close)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := attestations[strings.TrimPrefix(r.URL.Path, "/repos/actions/runner/attestations/")]
		if !ok || r.URL.Query().Get("predicate_type") != "provenance" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(api.Close)
	trusted, err := root.NewTrustedRootFromPath("testdata/trusted_root.json")
	if err != nil {
		t.Fatal(err)
	}
	return Checker{
		Registry:    registry.URL,
		API:         api.URL,
		TrustedRoot: func(context.Context) (root.TrustedMaterial, error) { return trusted, nil },
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// retarget rewrites the subject of the statement in an attestation from one
// digest to another, leaving its signature as it was.
func retarget(t *testing.T, attestations, from, to string) string {
	t.Helper()
	var answer struct {
		Attestations []struct {
			Bundle map[string]any `json:"bundle"`
		} `json:"attestations"`
	}
	if err := json.Unmarshal([]byte(attestations), &answer); err != nil {
		t.Fatal(err)
	}
	envelope := answer.Attestations[0].Bundle["dsseEnvelope"].(map[string]any)
	payload, err := base64.StdEncoding.DecodeString(envelope["payload"].(string))
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.ReplaceAll(string(payload), strings.TrimPrefix(from, "sha256:"), strings.TrimPrefix(to, "sha256:"))
	if changed == string(payload) {
		t.Fatal("the statement does not name " + from)
	}
	envelope["payload"] = base64.StdEncoding.EncodeToString([]byte(changed))
	out, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestCheckVerifiesTheRunnerImage(t *testing.T) {
	c := fake(t, map[string]string{"2.338.0": digest338}, map[string]string{digest338: read(t, "testdata/attestations-2.338.0.json")})
	got, err := c.Check(context.Background(), "2.338.0")
	if err != nil || got != digest338 {
		t.Errorf("Check = %q, %v; want %s", got, err, digest338)
	}
}

func TestCheckRefuses(t *testing.T) {
	bundle338 := read(t, "testdata/attestations-2.338.0.json")
	cases := []struct {
		name         string
		version      string
		tags         map[string]string
		attestations map[string]string
		ref, reason  string
	}{
		{
			name:    "an image without an attestation",
			version: "2.338.0", tags: map[string]string{"2.338.0": digest337},
			ref: "ghcr.io/actions/actions-runner@" + digest337, reason: "actions/runner published no attestation for it",
		},
		{
			name:    "an attestation for another image",
			version: "2.338.0", tags: map[string]string{"2.338.0": digest337}, attestations: map[string]string{digest337: bundle338},
			ref: "ghcr.io/actions/actions-runner@" + digest337, reason: "no attestation verifies: failed to verify signature: provided artifact digest does not match any digest in statement",
		},
		{
			name:    "an older release's image under a newer tag",
			version: "2.339.0", tags: map[string]string{"2.339.0": digest338}, attestations: map[string]string{digest338: bundle338},
			ref: "ghcr.io/actions/actions-runner@" + digest338, reason: "no attestation verifies: failed to verify certificate identity: no matching CertificateIdentity found, last error: expected SAN value \"https://github.com/actions/runner/.github/workflows/release.yml@refs/heads/releases/m339\", got \"https://github.com/actions/runner/.github/workflows/release.yml@refs/heads/releases/m338\"",
		},
		{
			name:    "a changed statement",
			version: "2.338.0", tags: map[string]string{"2.338.0": digest337}, attestations: map[string]string{digest337: retarget(t, bundle338, digest338, digest337)},
			ref: "ghcr.io/actions/actions-runner@" + digest337, reason: "no attestation verifies: failed to verify log inclusion: ",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := fake(t, tc.tags, tc.attestations).Check(context.Background(), tc.version)
			var refused *Refused
			if !errors.As(err, &refused) {
				t.Fatalf("Check = %v, want a refusal", err)
			}
			if refused.Ref != tc.ref || !strings.HasPrefix(refused.Reason, tc.reason) {
				t.Errorf("refused %s: %s\nwant %s: %s", refused.Ref, refused.Reason, tc.ref, tc.reason)
			}
		})
	}
}

// A registry or API that does not answer says nothing about the image, so the
// agent retries later instead of reporting a refusal.
func TestCheckDoesNotRefuseWhatItCouldNotReach(t *testing.T) {
	c := fake(t, map[string]string{"2.338.0": digest338}, nil)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer down.Close()

	offline := c
	offline.Registry = down.URL
	_, err := offline.Check(context.Background(), "2.338.0")
	if want := down.URL + "/token answered 503"; err == nil || err.Error() != want {
		t.Errorf("Check with the registry down = %v, want %s", err, want)
	}

	offline = c
	offline.API = down.URL
	_, err = offline.Check(context.Background(), "2.338.0")
	if want := down.URL + "/repos/actions/runner/attestations/" + digest338 + " answered 503"; err == nil || err.Error() != want {
		t.Errorf("Check with the API down = %v, want %s", err, want)
	}

	_, err = c.Check(context.Background(), "2.340.0")
	if want := c.Registry + "/v2/actions/actions-runner/manifests/2.340.0 answered 404"; err == nil || err.Error() != want {
		t.Errorf("Check of a tag the registry lacks = %v, want %s", err, want)
	}

	var refused *Refused
	if _, err := c.Check(context.Background(), "latest"); errors.As(err, &refused) || err == nil || err.Error() != `runner version "latest" is not major.minor.patch` {
		t.Errorf("Check of latest = %v", err)
	}
}

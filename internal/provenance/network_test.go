package provenance

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/release"
)

// TestCheckAgainstGitHub checks the latest runner release's image against
// ghcr.io, GitHub's attestation API and Sigstore's TUF repository. It runs
// only with BOXRUNNER_NETWORK set.
func TestCheckAgainstGitHub(t *testing.T) {
	if os.Getenv("BOXRUNNER_NETWORK") == "" {
		t.Skip("set BOXRUNNER_NETWORK to check the latest runner image against GitHub and Sigstore")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	version, err := release.Latest(ctx, http.DefaultClient, "https://github.com/actions/runner/releases/latest")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := Checker{}.Check(ctx, version)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s:%s is %s", Image, version, digest)
}

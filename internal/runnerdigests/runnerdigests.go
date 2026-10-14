// Package runnerdigests chooses the runner image a box builds from: a version
// and digest from runner-digests.json, the list of actions/runner images whose
// provenance boxrunner's CI verified (.github/workflows/runner-digests.yml).
package runnerdigests

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/release"
)

const (
	// Image is the runner image's repository.
	Image = "ghcr.io/actions/actions-runner"
	// URL is where the CI workflow publishes the list.
	URL = "https://raw.githubusercontent.com/dominikzabcik/boxrunner/digests/runner-digests.json"
	// ReleasesLatest is the runner's latest-release page, for release.Latest.
	ReleasesLatest = "https://github.com/actions/runner/releases/latest"

	registry = "https://ghcr.io"
)

var (
	versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	digestPattern  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// List maps each runner version whose image boxrunner's CI verified to that
// image's digest, the multi-platform index its tag pointed to.
type List map[string]string

// Choice is the runner image a box builds.
type Choice struct {
	Version, Digest string
	// Unlisted is the runner's latest release when the list does not have it
	// yet; Version is then the newest release before it that the list has.
	Unlisted string
}

// Choose picks the latest release when the list has it, and otherwise the
// newest listed release before it, so a box never builds an image the CI has
// not verified.
func (l List) Choose(latest string) (Choice, error) {
	var c Choice
	for version, digest := range l {
		if release.Compare(version, latest) <= 0 && (c.Version == "" || release.Newer(version, c.Version)) {
			c.Version, c.Digest = version, digest
		}
	}
	if c.Version == "" {
		return Choice{}, fmt.Errorf("boxrunner's list of verified runner images has no release up to %s", latest)
	}
	if c.Version != latest {
		c.Unlisted = latest
	}
	return c, nil
}

// Mismatch is the error for a version whose tag points to another image than
// the one the list names. The agent builds nothing then.
type Mismatch struct{ Version, Listed, Resolved string }

func (e *Mismatch) Error() string {
	return fmt.Sprintf("%s:%s points to %s, not to %s that boxrunner's CI verified", Image, e.Version, e.Resolved, e.Listed)
}

// Source reads the runner's latest release, the list and the registry. Its
// zero value talks to github.com, the published list and ghcr.io.
type Source struct {
	HTTP *http.Client
	// Releases, List and Registry replace the runner's latest-release page,
	// URL and ghcr.io in tests.
	Releases, List, Registry string
}

func (s Source) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Choose finds the runner's latest release and chooses from the list for it.
func (s Source) Choose(ctx context.Context) (Choice, error) {
	latest, err := release.Latest(ctx, s.client(), cmp.Or(s.Releases, ReleasesLatest))
	if err != nil {
		return Choice{}, err
	}
	list, err := s.Fetch(ctx)
	if err != nil {
		return Choice{}, err
	}
	return list.Choose(latest)
}

// Fetch downloads the list and checks that it holds only versions and digests.
func (s Source) Fetch(ctx context.Context) (List, error) {
	url := cmp.Or(s.List, URL)
	var list List
	if _, err := s.get(ctx, http.MethodGet, url, nil, &list); err != nil {
		return nil, fmt.Errorf("the list of verified runner images: %w", err)
	}
	for version, digest := range list {
		if !versionPattern.MatchString(version) || !digestPattern.MatchString(digest) {
			return nil, fmt.Errorf("the list of verified runner images at %s has %q: %q, not a version and a sha256 digest", url, version, digest)
		}
	}
	return list, nil
}

// Confirm checks that the version's tag still points to the digest the list
// names, and returns a *Mismatch when it does not.
func (s Source) Confirm(ctx context.Context, version, digest string) error {
	resolved, err := s.Resolve(ctx, version)
	if err != nil {
		return err
	}
	if resolved != digest {
		return &Mismatch{Version: version, Listed: digest, Resolved: resolved}
	}
	return nil
}

// Resolve asks the registry, anonymously, which digest the version's tag
// points to: the multi-platform index, as the CI workflow resolves it.
func (s Source) Resolve(ctx context.Context, version string) (string, error) {
	base := cmp.Or(s.Registry, registry)
	repo := strings.TrimPrefix(Image, "ghcr.io/")
	var token struct {
		Token string `json:"token"`
	}
	if _, err := s.get(ctx, http.MethodGet, base+"/token?scope=repository:"+repo+":pull", nil, &token); err != nil {
		return "", err
	}
	header, err := s.get(ctx, http.MethodHead, base+"/v2/"+repo+"/manifests/"+version, map[string]string{
		"Authorization": "Bearer " + token.Token,
		"Accept":        "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json",
	}, nil)
	if err != nil {
		return "", err
	}
	digest := header.Get("Docker-Content-Digest")
	if !digestPattern.MatchString(digest) {
		return "", fmt.Errorf("%s:%s: the registry named no sha256 digest (%q)", Image, version, digest)
	}
	return digest, nil
}

// get sends a request and decodes a JSON answer into out, when out is not nil.
func (s Source) get(ctx context.Context, method, url string, header map[string]string, out any) (http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", strings.SplitN(url, "?", 2)[0], resp.Status)
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
			return nil, fmt.Errorf("%s: %w", url, err)
		}
	}
	return resp.Header, nil
}

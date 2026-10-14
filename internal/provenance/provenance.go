// Package provenance finds the digest of a runner version's image in
// ghcr.io/actions/actions-runner and checks, with Sigstore, that
// actions/runner's release workflow built it.
package provenance

import (
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

const (
	// Image is the runner image's repository.
	Image = "ghcr.io/actions/actions-runner"

	registry = "https://ghcr.io"
	api      = "https://api.github.com"
	issuer   = "https://token.actions.githubusercontent.com"
	slsa     = "https://slsa.dev/provenance/v1"
)

// Checker finds and checks runner images. Its zero value talks to ghcr.io,
// api.github.com and Sigstore.
type Checker struct {
	HTTP *http.Client
	// Registry and API replace ghcr.io and api.github.com in tests.
	Registry, API string
	// TrustedRoot returns Sigstore's certificate authorities and log keys.
	// Nil fetches them through Sigstore's TUF repository.
	TrustedRoot func(context.Context) (root.TrustedMaterial, error)
}

// Refused is the error for a runner image whose provenance does not hold.
// Other errors, such as an unreachable registry, say nothing about the image.
type Refused struct {
	// Ref is the image by digest, such as ghcr.io/actions/actions-runner@sha256:….
	Ref    string
	Reason string
}

func (e *Refused) Error() string { return e.Ref + ": " + e.Reason }

var (
	versionPattern = regexp.MustCompile(`^[0-9]+\.([0-9]+)\.[0-9]+$`)
	digestPattern  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// Signer is the identity in the certificate that signs a runner version's
// provenance: actions/runner's release workflow, run in actions/runner on
// the release branch of the version's minor release, such as releases/m338
// for 2.338.0. Binding the branch to the version keeps a registry from
// passing off an older release's image.
func Signer(version string) (verify.CertificateIdentity, error) {
	m := versionPattern.FindStringSubmatch(version)
	if m == nil {
		return verify.CertificateIdentity{}, fmt.Errorf("runner version %q is not major.minor.patch", version)
	}
	ref := "refs/heads/releases/m" + m[1]
	// The SAN names the workflow file, which another repository could call
	// if it ever allowed workflow_call; the source repository cannot be
	// borrowed that way.
	san, err := verify.NewSANMatcher("https://github.com/actions/runner/.github/workflows/release.yml@"+ref, "")
	if err != nil {
		return verify.CertificateIdentity{}, err
	}
	iss, err := verify.NewIssuerMatcher(issuer, "")
	if err != nil {
		return verify.CertificateIdentity{}, err
	}
	return verify.NewCertificateIdentity(san, iss, certificate.Extensions{
		SourceRepositoryURI: "https://github.com/actions/runner",
		SourceRepositoryRef: ref,
		RunnerEnvironment:   "github-hosted",
	})
}

// Check returns the digest the registry gives the runner image tagged
// version, once a SLSA provenance attestation for that digest verifies
// against Sigstore and Signer(version).
func (c Checker) Check(ctx context.Context, version string) (string, error) {
	signer, err := Signer(version)
	if err != nil {
		return "", err
	}
	digest, err := c.digest(ctx, version)
	if err != nil {
		return "", err
	}
	ref := Image + "@" + digest
	bundles, err := c.bundles(ctx, digest)
	if err != nil {
		return "", err
	}
	if len(bundles) == 0 {
		return "", &Refused{ref, "actions/runner published no attestation for it"}
	}
	trusted, err := c.trustedRoot(ctx)
	if err != nil {
		return "", fmt.Errorf("fetching Sigstore's trusted root: %w", err)
	}
	var reasons []string
	for _, b := range bundles {
		err := check(trusted, b, digest, signer)
		if err == nil {
			return digest, nil
		}
		reasons = append(reasons, err.Error())
	}
	return "", &Refused{ref, "no attestation verifies: " + strings.Join(reasons, "; ")}
}

func check(trusted root.TrustedMaterial, raw json.RawMessage, digest string, signer verify.CertificateIdentity) error {
	var b bundle.Bundle
	if err := b.UnmarshalJSON(raw); err != nil {
		return err
	}
	v, err := verify.NewVerifier(trusted, verify.WithSignedCertificateTimestamps(1), verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1))
	if err != nil {
		return err
	}
	sum, _ := hex.DecodeString(strings.TrimPrefix(digest, "sha256:"))
	result, err := v.Verify(&b, verify.NewPolicy(verify.WithArtifactDigest("sha256", sum), verify.WithCertificateIdentity(signer)))
	if err != nil {
		return err
	}
	if result.Statement == nil || result.Statement.PredicateType != slsa {
		return errors.New("the attestation is not SLSA provenance")
	}
	return nil
}

func (c Checker) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// digest asks the registry, anonymously, which digest the tag points to: the
// multi-platform index the attestation names.
func (c Checker) digest(ctx context.Context, version string) (string, error) {
	base := cmp.Or(c.Registry, registry)
	repo := strings.TrimPrefix(Image, "ghcr.io/")
	var token struct {
		Token string `json:"token"`
	}
	if _, err := c.get(ctx, http.MethodGet, base+"/token?scope=repository:"+repo+":pull", nil, &token); err != nil {
		return "", err
	}
	header, err := c.get(ctx, http.MethodHead, base+"/v2/"+repo+"/manifests/"+version, map[string]string{
		"Authorization": "Bearer " + token.Token,
		"Accept":        "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json",
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

// bundles returns the Sigstore bundles of the attestations actions/runner
// published for a digest. The API needs no token for a public repository.
func (c Checker) bundles(ctx context.Context, digest string) ([]json.RawMessage, error) {
	var out struct {
		Attestations []struct {
			Bundle json.RawMessage `json:"bundle"`
		} `json:"attestations"`
	}
	url := cmp.Or(c.API, api) + "/repos/actions/runner/attestations/" + digest + "?predicate_type=provenance"
	_, err := c.get(ctx, http.MethodGet, url, map[string]string{"Accept": "application/vnd.github+json"}, &out)
	var status statusError
	if errors.As(err, &status) && status.code == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var bundles []json.RawMessage
	for _, a := range out.Attestations {
		bundles = append(bundles, a.Bundle)
	}
	return bundles, nil
}

func (c Checker) trustedRoot(ctx context.Context) (root.TrustedMaterial, error) {
	if c.TrustedRoot != nil {
		return c.TrustedRoot(ctx)
	}
	// Builds are rare, so the agent fetches the root afresh from the TUF root
	// embedded in sigstore-go instead of keeping a cache on the box.
	return root.FetchTrustedRootWithOptions(tuf.DefaultOptions().WithDisableLocalCache().WithContext(ctx))
}

type statusError struct {
	url  string
	code int
}

func (e statusError) Error() string { return fmt.Sprintf("%s answered %d", e.url, e.code) }

// get sends a request and decodes a JSON answer into out, when out is not nil.
func (c Checker) get(ctx context.Context, method, url string, header map[string]string, out any) (http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusError{strings.SplitN(url, "?", 2)[0], resp.StatusCode}
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil {
			return nil, fmt.Errorf("%s: %w", url, err)
		}
	}
	return resp.Header, nil
}

// Package runnerimage builds the job image and finds the runner's latest
// release.
package runnerimage

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/dominikzabcik/boxrunner/internal/config"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/dominikzabcik/boxrunner/image"
	"github.com/dominikzabcik/boxrunner/internal/docker"
	"github.com/dominikzabcik/boxrunner/internal/layout"
)

const ReleasesLatest = "https://github.com/actions/runner/releases/latest"

var versionPattern = regexp.MustCompile(`/releases/tag/v([0-9]+\.[0-9]+\.[0-9]+)$`)

// ParseLatest reads the runner version from where the latest-release page
// redirects, such as .../releases/tag/v2.329.0.
func ParseLatest(location string) (string, error) {
	m := versionPattern.FindStringSubmatch(location)
	if m == nil {
		return "", fmt.Errorf("latest runner release: unexpected redirect %q", location)
	}
	return m[1], nil
}

// Latest asks github.com for the runner's latest release through the page's
// redirect, which needs no API quota.
func Latest(ctx context.Context, client *http.Client, url string) (string, error) {
	noFollow := *client
	noFollow.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := noFollow.Do(req)
	if err != nil {
		return "", fmt.Errorf("latest runner release: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 3 {
		return "", fmt.Errorf("latest runner release: %s answered %s", url, resp.Status)
	}
	return ParseLatest(resp.Header.Get("Location"))
}

// Tar is the embedded build context as a tar stream.
func Tar() (*bytes.Buffer, error) {
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	err := fs.WalkDir(image.Context, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(image.Context, path)
		if err != nil {
			return err
		}
		mode := int64(0o644)
		if strings.HasPrefix(string(data), "#!") {
			mode = 0o755
		}
		if err := w.WriteHeader(&tar.Header{Name: path, Mode: mode, Size: int64(len(data))}); err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &buf, w.Close()
}

// Identity names a job image by the runner version and the build context
// embedded in this binary, such as 2.338.0+3f9a2c0b1d4e, so a new boxrunner
// release with a changed Dockerfile rebuilds the image too.
func Identity(runnerVersion string) string {
	buf, err := Tar()
	if err != nil {
		panic(err) // the embedded context is part of the binary
	}
	sum := sha256.Sum256(buf.Bytes())
	return runnerVersion + "+" + hex.EncodeToString(sum[:6])
}

// Build builds the job image for an identity from Identity.
func Build(ctx context.Context, d *docker.Client, identity string) error {
	buf, err := Tar()
	if err != nil {
		return err
	}
	version, _, _ := strings.Cut(identity, "+")
	return d.Build(ctx, buf, docker.BuildOptions{
		Tag:    layout.Image,
		Args:   map[string]string{"RUNNER_VERSION": version},
		Labels: map[string]string{layout.LabelImage: identity},
		Pull:   true,
	})
}

// Labels that tie a class image to what it was built from.
const (
	LabelBase    = "boxrunner.base"
	LabelContext = "boxrunner.context"
)

// DirTar packs a build context directory into a tar stream with fixed
// metadata, and returns the SHA-256 of that stream, so the same files always
// give the same hash.
func DirTar(dir string) (*bytes.Buffer, string, error) {
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := w.WriteHeader(&tar.Header{Name: filepath.ToSlash(rel), Mode: int64(info.Mode().Perm()), Size: int64(len(data))}); err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	})
	if err == nil {
		err = w.Close()
	}
	if err != nil {
		return nil, "", fmt.Errorf("build context %s: %w", dir, err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return &buf, hex.EncodeToString(sum[:]), nil
}

// EnsureClasses builds the image of every class that has a dockerfile, when
// it is missing or was built from another job image or other files. It
// returns the classes it built.
func EnsureClasses(ctx context.Context, d *docker.Client, classes []config.Class) ([]string, error) {
	base, ok, err := d.Image(ctx, layout.Image)
	if err != nil || !ok {
		return nil, cmp.Or(err, errors.New("the job image is not built yet"))
	}
	var built []string
	for _, c := range classes {
		if c.Dockerfile == "" {
			continue
		}
		buf, hash, err := DirTar(filepath.Dir(c.Dockerfile))
		if err != nil {
			return built, fmt.Errorf("class %s: %w", c.Name, err)
		}
		img, ok, err := d.Image(ctx, c.Image)
		if err != nil {
			return built, err
		}
		if ok && img.Labels[LabelBase] == base.ID && img.Labels[LabelContext] == hash {
			continue
		}
		err = d.Build(ctx, buf, docker.BuildOptions{
			Tag:        c.Image,
			Dockerfile: filepath.Base(c.Dockerfile),
			Args:       map[string]string{"BASE": layout.Image},
			Labels:     map[string]string{LabelBase: base.ID, LabelContext: hash},
		})
		if err != nil {
			return built, fmt.Errorf("class %s: %w", c.Name, err)
		}
		built = append(built, c.Name)
	}
	return built, nil
}

// BaseRepo is the official runner image the job image is built from.
const BaseRepo = "ghcr.io/actions/actions-runner"

// baseTag matches the tags the job image's build pulls, one per runner
// version; a tag like latest is someone else's.
var baseTag = regexp.MustCompile(`^` + regexp.QuoteMeta(BaseRepo) + `:[0-9]+\.[0-9]+\.[0-9]+$`)

func tagged(img docker.ImageSummary) bool {
	return slices.ContainsFunc(img.RepoTags, func(t string) bool { return t != "<none>:<none>" })
}

// Superseded returns what rebuilds left behind, as references to remove in
// order: job and class images no tag points to any more, then the runner
// images of other runner versions. An image a container uses, running or
// not, stays. Docker lists no untagged image another image is built from,
// and removing an image removes such parents with it, so the old job image
// goes with the last old class image built from it.
func Superseded(images []docker.ImageSummary, containers []docker.Container, runnerVersion string) []string {
	used := map[string]bool{}
	for _, c := range containers {
		used[c.ImageID] = true
	}
	var dangling, versions []string
	for _, img := range images {
		if used[img.ID] {
			continue
		}
		if !tagged(img) && img.Labels[layout.LabelImage] != "" {
			dangling = append(dangling, img.ID)
		}
		for _, tag := range img.RepoTags {
			if baseTag.MatchString(tag) && tag != BaseRepo+":"+runnerVersion {
				versions = append(versions, tag)
			}
		}
	}
	slices.Sort(dangling)
	slices.Sort(versions)
	return append(dangling, versions...)
}

// DiskUse is what boxrunner's images take on disk: the job and class
// images, which carry the job image's identity, and the runner images they
// are built from. Each counts only the layers it adds to the nearest listed
// image it was built from: a class image's job image, or else the runner
// image of its runner version.
func DiskUse(images []docker.ImageSummary) int64 {
	byID := map[string]docker.ImageSummary{}
	byTag := map[string]docker.ImageSummary{}
	for _, img := range images {
		byID[img.ID] = img
		for _, tag := range img.RepoTags {
			byTag[tag] = img
		}
	}
	var total int64
	for _, img := range images {
		identity := img.Labels[layout.LabelImage]
		if identity == "" && !slices.ContainsFunc(img.RepoTags, baseTag.MatchString) {
			continue
		}
		total += img.Size
		version, _, _ := strings.Cut(identity, "+")
		if base, ok := byID[img.Labels[LabelBase]]; ok {
			total -= base.Size
		} else if base, ok := byTag[BaseRepo+":"+version]; ok && identity != "" {
			total -= base.Size
		}
	}
	return total
}

// Prune removes the images Superseded names and returns the references it
// removed and the disk boxrunner's images use afterwards. It goes on past an
// image Docker refuses to remove.
func Prune(ctx context.Context, d *docker.Client) ([]string, int64, error) {
	identity, err := Built(ctx, d)
	if err != nil || identity == "" {
		return nil, 0, cmp.Or(err, errors.New("the job image is not built yet"))
	}
	images, err := d.Images(ctx)
	if err != nil {
		return nil, 0, err
	}
	containers, err := d.List(ctx, "", false)
	if err != nil {
		return nil, 0, err
	}
	version, _, _ := strings.Cut(identity, "+")
	var removed []string
	var errs []error
	for _, ref := range Superseded(images, containers, version) {
		if err := d.RemoveImage(ctx, ref); err != nil {
			errs = append(errs, fmt.Errorf("removing image %s: %w", ref, err))
			continue
		}
		removed = append(removed, ref)
	}
	if len(removed) > 0 {
		if images, err = d.Images(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return removed, DiskUse(images), errors.Join(errs...)
}

// Built returns the identity of the job image, or "" when there is none or it
// predates identities, so the agent builds it again.
func Built(ctx context.Context, d *docker.Client) (string, error) {
	labels, ok, err := d.ImageLabels(ctx, layout.Image)
	if err != nil || !ok {
		return "", err
	}
	return labels[layout.LabelImage], nil
}

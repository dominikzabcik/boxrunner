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

// Build builds the job image for a runner version.
func Build(ctx context.Context, d *docker.Client, version string) error {
	buf, err := Tar()
	if err != nil {
		return err
	}
	return d.Build(ctx, buf, docker.BuildOptions{Tag: layout.Image, Args: map[string]string{"RUNNER_VERSION": version}, Pull: true})
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

// Built returns the runner version of the job image, or "" when there is none.
func Built(ctx context.Context, d *docker.Client) (string, error) {
	labels, ok, err := d.ImageLabels(ctx, layout.Image)
	if err != nil || !ok {
		return "", err
	}
	v := labels[layout.LabelVersion]
	if v == "" {
		return "", errors.New("the job image has no runner version label")
	}
	return v, nil
}

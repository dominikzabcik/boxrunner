// Package runnerimage builds the job image and finds the runner's latest
// release.
package runnerimage

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
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
	return d.Build(ctx, buf, layout.Image, map[string]string{"RUNNER_VERSION": version})
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

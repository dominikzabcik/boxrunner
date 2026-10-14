// Package release finds a project's latest release on github.com and compares
// versions.
package release

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// Repo is boxrunner's repository; its releases hold the installer.
const Repo = "dominikzabcik/boxrunner"

// Releases is where boxrunner's releases live.
const Releases = "https://github.com/" + Repo + "/releases"

var tagPattern = regexp.MustCompile(`/releases/tag/v([0-9]+\.[0-9]+\.[0-9]+)$`)

// ParseLatest reads the version from where a latest-release page redirects,
// such as .../releases/tag/v2.329.0.
func ParseLatest(location string) (string, error) {
	m := tagPattern.FindStringSubmatch(location)
	if m == nil {
		return "", fmt.Errorf("latest release: unexpected redirect %q", location)
	}
	return m[1], nil
}

// Latest asks github.com for a project's latest release through the redirect
// of its latest-release page (url), which needs no API quota.
func Latest(ctx context.Context, client *http.Client, url string) (string, error) {
	noFollow := *client
	noFollow.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := noFollow.Do(req)
	if err != nil {
		return "", fmt.Errorf("latest release: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 3 {
		return "", fmt.Errorf("latest release: %s answered %s", url, resp.Status)
	}
	return ParseLatest(resp.Header.Get("Location"))
}

// Compare orders two versions such as 0.2.1 and 0.3.0-rc.1 by their numbers.
// A pre-release or development build comes before the release it leads to.
func Compare(a, b string) int {
	an, apre := parse(a)
	bn, bpre := parse(b)
	for i := range an {
		if an[i] != bn[i] {
			if an[i] < bn[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case apre == bpre:
		return 0
	case apre == "":
		return 1
	case bpre == "":
		return -1
	}
	return strings.Compare(apre, bpre)
}

// Newer reports whether version a comes after b. An empty version is never
// newer, and nothing is newer than an empty one.
func Newer(a, b string) bool {
	return a != "" && b != "" && Compare(a, b) > 0
}

func parse(v string) (numbers [3]int, pre string) {
	v = strings.TrimPrefix(v, "v")
	v, _, _ = strings.Cut(v, "+")
	v, pre, _ = strings.Cut(v, "-")
	for i, part := range strings.SplitN(v, ".", 3) {
		numbers[i], _ = strconv.Atoi(part)
	}
	return numbers, pre
}

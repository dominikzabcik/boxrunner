// Package upgrade installs another boxrunner release on the box. It runs that
// release's own installer, which checks the binary against the checksums
// written into it and, with gh signed in, the binary's build attestation, then
// runs `boxrunner install` with the box's existing configuration.
package upgrade

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dominikzabcik/boxrunner/internal/release"
)

type Options struct {
	// Version is this agent's.
	Version string
	// Target is the release to install, such as 0.2.2; empty means the latest.
	Target string
	// Downgrade allows a Target older than Version.
	Downgrade bool
	// Releases is where the releases live, release.Releases outside tests.
	Releases string
	HTTP     *http.Client
	// Exec runs a command and returns its output; it runs gh.
	Exec func(ctx context.Context, stdin, name string, args ...string) (string, error)
	// Install runs the downloaded installer.
	Install func(ctx context.Context, script string) error
	Out     io.Writer
}

var versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`)

// Run downloads the installer of the target release, checks it and runs it.
func Run(ctx context.Context, o Options) error {
	target := strings.TrimPrefix(o.Target, "v")
	if target == "" {
		latest, err := release.Latest(ctx, o.HTTP, o.Releases+"/latest")
		if err != nil {
			return err
		}
		target = latest
	}
	if !versionPattern.MatchString(target) {
		return fmt.Errorf("%q is not a release version such as 0.2.2", o.Target)
	}
	switch c := release.Compare(target, o.Version); {
	case c == 0:
		fmt.Fprintf(o.Out, "boxrunner %s is already installed.\n", target)
		return nil
	case c < 0 && !o.Downgrade:
		return fmt.Errorf("%s is older than this agent (%s); pass --downgrade to install it anyway", target, o.Version)
	}

	dir, err := os.MkdirTemp("", "boxrunner-upgrade-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	script := filepath.Join(dir, "install.sh")
	url := o.Releases + "/download/v" + target + "/install.sh"
	fmt.Fprintf(o.Out, "==> download the installer of boxrunner %s\n", target)
	body, err := download(ctx, o.HTTP, url)
	if err != nil {
		return err
	}
	if !strings.Contains(body, "\nVERSION=\""+target+"\"\n") {
		return fmt.Errorf("%s does not install version %s; nothing was installed", url, target)
	}
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		return err
	}
	if _, err := o.Exec(ctx, "", "gh", "auth", "status"); err != nil {
		fmt.Fprintln(o.Out, "==> skipped the installer's build attestation check (needs gh, signed in)")
	} else if out, err := o.Exec(ctx, "", "gh", "attestation", "verify", script, "--repo", release.Repo); err != nil {
		return fmt.Errorf("the build attestation does not match the installer of %s; nothing was installed: %s", target, strings.TrimSpace(out))
	} else {
		fmt.Fprintln(o.Out, "==> the installer's build attestation ok")
	}
	return o.Install(ctx, script)
}

func download(ctx context.Context, client *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s answered %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(body), err
}

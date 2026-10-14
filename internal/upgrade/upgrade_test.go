package upgrade

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
)

func installer(version string) string {
	return "#!/bin/sh\nset -eu\n\nVERSION=\"" + version + "\"\nCHECKSUMS=\"…\"\n"
}

// releases serves the latest-release redirect and the installers of 0.2.1
// and 0.2.2; 0.2.3's installer names the wrong version.
func releases(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			http.Redirect(w, r, "/releases/tag/v0.2.2", http.StatusFound)
		case "/releases/download/v0.2.1/install.sh":
			w.Write([]byte(installer("0.2.1")))
		case "/releases/download/v0.2.2/install.sh":
			w.Write([]byte(installer("0.2.2")))
		case "/releases/download/v0.2.3/install.sh":
			w.Write([]byte(installer("0.2.2")))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type run struct {
	gh        [][]string
	installed []string
	out       strings.Builder
}

// options runs the upgrade from version with gh signed in when ghState is
// "signed in", and its attestation check failing when it is "mismatch".
func (r *run) options(srv *httptest.Server, version, target, ghState string) Options {
	return Options{
		Version: version, Target: target, Releases: srv.URL + "/releases", HTTP: srv.Client(), Out: &r.out,
		Exec: func(_ context.Context, _, name string, args ...string) (string, error) {
			r.gh = append(r.gh, append([]string{name}, args[:2]...))
			switch {
			case ghState == "":
				return "", errors.New(`exec: "gh": executable file not found in $PATH`)
			case args[0] == "attestation" && ghState == "mismatch":
				return "✗ Verification failed", errors.New("exit status 1")
			}
			return "", nil
		},
		Install: func(_ context.Context, script string) error {
			body, err := os.ReadFile(script)
			r.installed = append(r.installed, string(body))
			return err
		},
	}
}

func TestUpgradeToTheLatestRelease(t *testing.T) {
	srv := releases(t)
	var r run
	if err := Run(context.Background(), r.options(srv, "0.2.1", "", "")); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.installed, []string{installer("0.2.2")}) {
		t.Errorf("installed %q", r.installed)
	}
	want := "==> download the installer of boxrunner 0.2.2\n==> skipped the installer's build attestation check (needs gh, signed in)\n"
	if r.out.String() != want {
		t.Errorf("output =\n%s\nwant\n%s", r.out.String(), want)
	}
}

func TestUpgradeChecksTheAttestationWithGh(t *testing.T) {
	srv := releases(t)
	var r run
	if err := Run(context.Background(), r.options(srv, "0.2.1", "v0.2.2", "signed in")); err != nil {
		t.Fatal(err)
	}
	if want := [][]string{{"gh", "auth", "status"}, {"gh", "attestation", "verify"}}; !reflect.DeepEqual(r.gh, want) {
		t.Errorf("gh calls %q", r.gh)
	}
	if len(r.installed) != 1 || !strings.HasSuffix(r.out.String(), "==> the installer's build attestation ok\n") {
		t.Errorf("installed %d times, output:\n%s", len(r.installed), r.out.String())
	}

	var bad run
	err := Run(context.Background(), bad.options(srv, "0.2.1", "0.2.2", "mismatch"))
	if err == nil || err.Error() != "the build attestation does not match the installer of 0.2.2; nothing was installed: ✗ Verification failed" {
		t.Errorf("Run with a mismatched attestation = %v", err)
	}
	if bad.installed != nil {
		t.Errorf("a mismatched installer ran: %q", bad.installed)
	}
}

func TestUpgradeRefusals(t *testing.T) {
	srv := releases(t)
	cases := []struct {
		name, version, target, want string
	}{
		{"a downgrade", "0.2.2", "0.2.1", "0.2.1 is older than this agent (0.2.2); pass --downgrade to install it anyway"},
		{"no such release", "0.2.1", "0.9.0", srv.URL + "/releases/download/v0.9.0/install.sh answered 404 Not Found"},
		{"an installer for another version", "0.2.1", "0.2.3", srv.URL + "/releases/download/v0.2.3/install.sh does not install version 0.2.3; nothing was installed"},
		{"not a version", "0.2.1", "../latest", `"../latest" is not a release version such as 0.2.2`},
	}
	for _, c := range cases {
		var r run
		err := Run(context.Background(), r.options(srv, c.version, c.target, ""))
		if err == nil || err.Error() != c.want || r.installed != nil {
			t.Errorf("%s: Run = %v, installed %q; want %q", c.name, err, r.installed, c.want)
		}
	}
}

func TestUpgradeDowngradesWhenAsked(t *testing.T) {
	srv := releases(t)
	var r run
	o := r.options(srv, "0.2.2", "0.2.1", "")
	o.Downgrade = true
	if err := Run(context.Background(), o); err != nil || !reflect.DeepEqual(r.installed, []string{installer("0.2.1")}) {
		t.Errorf("Run = %v, installed %q", err, r.installed)
	}
}

func TestUpgradeToTheInstalledReleaseDoesNothing(t *testing.T) {
	srv := releases(t)
	var r run
	if err := Run(context.Background(), r.options(srv, "0.2.2", "", "")); err != nil || r.installed != nil {
		t.Errorf("Run = %v, installed %q", err, r.installed)
	}
	if r.out.String() != "boxrunner 0.2.2 is already installed.\n" {
		t.Errorf("output %q", r.out.String())
	}
}

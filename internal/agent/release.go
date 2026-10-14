package agent

import (
	"context"
	"net/http"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/release"
)

// checkRelease looks for boxrunner's latest release daily, so status and the
// dashboard can say when a newer one is out.
func (a *Agent) checkRelease(ctx context.Context) {
	client := &http.Client{Timeout: 30 * time.Second}
	for {
		wait := imageEvery
		latest, err := release.Latest(ctx, client, a.BoxrunnerReleasesURL)
		if err != nil {
			a.Log.Warn("checking boxrunner's latest release", "err", err)
			wait = imageRetry
		} else {
			if release.Newer(latest, a.Version) {
				a.Log.Info("a newer boxrunner is out; upgrade with: sudo boxrunner upgrade", "version", a.Version, "latest", latest)
			}
			a.latest.Store(&latest)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

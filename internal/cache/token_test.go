package cache

import (
	"crypto/rsa"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// keyServer serves the test key until down is set, then answers 503. A read
// waits while hold is set.
type keyServer struct {
	reads      atomic.Int32
	down, hold atomic.Bool
	started    chan struct{}
	release    chan struct{}
	url        string
}

func newKeyServer(t *testing.T) *keyServer {
	good, _ := jwks(t, map[string]*rsa.PublicKey{"k1": &testKey.PublicKey})
	ks := &keyServer{started: make(chan struct{}, 10), release: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ks.reads.Add(1)
		if ks.hold.Load() {
			ks.started <- struct{}{}
			<-ks.release
		}
		if ks.down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		http.Redirect(w, r, good.URL, http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	ks.url = srv.URL
	return ks
}

// TestKeyServerOutage: with the keys a day old and their server down, calls
// go on with the keys the cache has and read them again only after a pause
// that grows with each failure.
func TestKeyServerOutage(t *testing.T) {
	ks := newKeyServer(t)
	k := &Keys{URL: ks.url, Issuer: Issuer, HTTP: http.DefaultClient}
	if _, err := k.Verify(t.Context(), mainToken, testTime); err != nil {
		t.Fatal(err)
	}
	ks.down.Store(true)
	var reads []int32
	for _, at := range []time.Duration{0, time.Second, time.Minute - time.Second, time.Minute, 2*time.Minute + time.Second, 3 * time.Minute} {
		if _, err := k.Verify(t.Context(), mainToken, testTime.Add(2*time.Hour+at)); err != nil {
			t.Fatalf("Verify at +%v: %v", at, err)
		}
		reads = append(reads, ks.reads.Load())
	}
	if want := []int32{2, 2, 2, 3, 3, 4}; !slices.Equal(reads, want) {
		t.Errorf("reads after each call = %v; want %v, a minute and then two after a failure", reads, want)
	}
}

// TestKeysFailClosed: with no keys and their server down, every call fails
// and only the first reads.
func TestKeysFailClosed(t *testing.T) {
	ks := newKeyServer(t)
	ks.down.Store(true)
	k := &Keys{URL: ks.url, Issuer: Issuer, HTTP: http.DefaultClient}
	for range 3 {
		_, err := k.Verify(t.Context(), mainToken, testTime)
		if err == nil || errors.Is(err, ErrToken) || err.Error() != "the token issuer's keys cannot be read" {
			t.Fatalf("Verify without keys = %v; want the keys cannot be read", err)
		}
	}
	if ks.reads.Load() != 1 {
		t.Errorf("%d reads; want 1", ks.reads.Load())
	}
}

// TestKeysReadOutsideTheLock: while one call reads the keys, others that
// know the key go on.
func TestKeysReadOutsideTheLock(t *testing.T) {
	ks := newKeyServer(t)
	k := &Keys{URL: ks.url, Issuer: Issuer, HTTP: http.DefaultClient}
	if _, err := k.Verify(t.Context(), mainToken, testTime); err != nil {
		t.Fatal(err)
	}
	ks.hold.Store(true)
	later := testTime.Add(2 * time.Hour)
	reading := make(chan error, 1)
	go func() {
		_, err := k.Verify(t.Context(), mainToken, later)
		reading <- err
	}()
	<-ks.started
	other := make(chan error, 1)
	go func() {
		_, err := k.Verify(t.Context(), mainToken, later)
		other <- err
	}()
	select {
	case err := <-other:
		if err != nil {
			t.Errorf("Verify during a read: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Errorf("Verify waited for another call's read of the keys")
	}
	close(ks.release)
	if err := <-reading; err != nil {
		t.Errorf("the reading call: %v", err)
	}
	if ks.reads.Load() != 2 {
		t.Errorf("%d reads; want 2", ks.reads.Load())
	}
}

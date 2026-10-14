package cache

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	testKey  = mustKey()
	testTime = time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
)

func mustKey() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
}

// sign makes an RS256 token with the test key.
func sign(key *rsa.PrivateKey, kid string, claims map[string]any) string {
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"})
	p, _ := json.Marshal(claims)
	in := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(p)
	sum := sha256.Sum256([]byte(in))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	return in + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// runtimeToken is a token as GitHub gives a job of repository repo, with
// scopes such as "refs/heads/main:3".
func runtimeToken(repo string, scopes ...string) string {
	var ac []map[string]any
	for _, s := range scopes {
		name, perm, _ := strings.Cut(s, ":")
		ac = append(ac, map[string]any{"Scope": name, "Permission": int(perm[0] - '0')})
	}
	b, _ := json.Marshal(ac)
	return sign(testKey, "k1", map[string]any{
		"iss": Issuer, "nbf": testTime.Add(-time.Minute).Unix(), "exp": testTime.Add(6 * time.Hour).Unix(),
		"repository_id": repo, "ac": string(b),
	})
}

var (
	mainToken  = runtimeToken("100", "refs/heads/main:3")
	prToken    = runtimeToken("100", "refs/pull/7/merge:3", "refs/heads/main:1")
	otherToken = runtimeToken("200", "refs/heads/main:3")
)

func jwks(t *testing.T, keys map[string]*rsa.PublicKey) (*httptest.Server, *int) {
	fetches := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches++
		var set []map[string]string
		for kid, k := range keys {
			set = append(set, map[string]string{
				"kty": "RSA", "kid": kid, "alg": "RS256", "use": "sig",
				"n": base64.RawURLEncoding.EncodeToString(k.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.E)).Bytes()),
			})
		}
		json.NewEncoder(w).Encode(map[string]any{"keys": set})
	}))
	t.Cleanup(srv.Close)
	return srv, &fetches
}

type box struct {
	t     *testing.T
	srv   *httptest.Server
	store *Store
	free  int64
	mu    sync.Mutex
	now   time.Time
	// upstream records what reached GitHub's side.
	upstream []string
}

func newBox(t *testing.T, max int64) *box {
	b := &box{t: t, free: 100 << 30, now: testTime}
	keys, _ := jwks(t, map[string]*rsa.PublicKey{"k1": &testKey.PublicKey})
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.upstream = append(b.upstream, fmt.Sprintf("%s %s%s %s", r.Method, r.Host, r.URL.Path, r.Header.Get("Authorization")))
		b.mu.Unlock()
		fmt.Fprintf(w, `{"echo":%q}`, body)
	}))
	t.Cleanup(github.Close)
	b.store = &Store{Dir: t.TempDir(), Max: max, Floor: 10 << 30, Free: func() (int64, error) {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.free, nil
	}}
	if err := b.store.Open(); err != nil {
		t.Fatal(err)
	}
	up, _ := url.Parse(github.URL)
	s := &Server{Store: b.store, Keys: &Keys{URL: keys.URL, Issuer: Issuer, HTTP: http.DefaultClient}, Upstream: up,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: b.clock}
	b.srv = httptest.NewServer(s.Handler())
	s.Base = b.srv.URL
	t.Cleanup(b.srv.Close)
	return b
}

func (b *box) clock() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.now
}

func (b *box) advance(d time.Duration) {
	b.mu.Lock()
	b.now = b.now.Add(d)
	b.mu.Unlock()
}

// call makes a Twirp call and returns the status and the decoded answer.
func (b *box) call(token, method string, body any) (int, map[string]any) {
	b.t.Helper()
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, b.srv.URL+"/twirp/github.actions.results.api.v1.CacheService/"+method, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (b *box) put(url string, body string) int {
	b.t.Helper()
	req, _ := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	req.Header.Set("x-ms-blob-type", "BlockBlob")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (b *box) get(url string) (int, string) {
	b.t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// save stores an archive as actions/cache does: reserve, upload at once,
// finalize.
func (b *box) save(token, key, version, archive string) {
	b.t.Helper()
	status, out := b.call(token, "CreateCacheEntry", map[string]string{"key": key, "version": version})
	if status != 200 || out["ok"] != true {
		b.t.Fatalf("CreateCacheEntry(%s) = %d %v", key, status, out)
	}
	if got := b.put(out["signed_upload_url"].(string), archive); got != http.StatusCreated {
		b.t.Fatalf("upload of %s = %d", key, got)
	}
	status, out = b.call(token, "FinalizeCacheEntryUpload", map[string]string{"key": key, "version": version, "size_bytes": fmt.Sprint(len(archive))})
	if status != 200 || out["ok"] != true {
		b.t.Fatalf("FinalizeCacheEntryUpload(%s) = %d %v", key, status, out)
	}
}

// restore looks a key up as actions/cache does and downloads the match. It
// returns the matched key and the archive, or "" when nothing matched.
func (b *box) restore(token, key, version string, restoreKeys ...string) (string, string) {
	b.t.Helper()
	status, out := b.call(token, "GetCacheEntryDownloadURL", map[string]any{"key": key, "restore_keys": restoreKeys, "version": version})
	if status != 200 {
		b.t.Fatalf("GetCacheEntryDownloadURL(%s) = %d %v", key, status, out)
	}
	if out["ok"] != true {
		return "", ""
	}
	code, body := b.get(out["signed_download_url"].(string))
	if code != 200 {
		b.t.Fatalf("download of %s = %d", key, code)
	}
	return out["matched_key"].(string), body
}

func TestSaveAndRestore(t *testing.T) {
	b := newBox(t, 1<<30)
	b.save(mainToken, "deps-linux-abc", "v1", "archive of deps")
	key, archive := b.restore(mainToken, "deps-linux-abc", "v1")
	if key != "deps-linux-abc" || archive != "archive of deps" {
		t.Errorf("restore = %q %q; want deps-linux-abc and its archive", key, archive)
	}
	if key, _ := b.restore(mainToken, "deps-linux-abc", "v2"); key != "" {
		t.Errorf("restore of another version matched %q", key)
	}
	if size, n := b.store.Size(); size != 15 || n != 1 {
		t.Errorf("Size = %d bytes in %d entries; want 15 in 1", size, n)
	}
}

func TestBlockUpload(t *testing.T) {
	b := newBox(t, 1<<30)
	_, out := b.call(mainToken, "CreateCacheEntry", map[string]string{"key": "big", "version": "v1"})
	u := out["signed_upload_url"].(string)
	ids := []string{"QUFBQQ==", "QkJCQg==", "Q0NDQw=="}
	for i, part := range []string{"one ", "two ", "three"} {
		// Out of order, as concurrent uploads arrive.
		j := 2 - i
		if got := b.put(u+"?comp=block&blockid="+url.QueryEscape(ids[j]), []string{"one ", "two ", "three"}[j]); got != http.StatusCreated {
			t.Fatalf("put block %s = %d", part, got)
		}
	}
	list := `<?xml version="1.0" encoding="utf-8"?><BlockList><Latest>QUFBQQ==</Latest><Latest>QkJCQg==</Latest><Latest>Q0NDQw==</Latest></BlockList>`
	if got := b.put(u+"?comp=blocklist", list); got != http.StatusCreated {
		t.Fatalf("put block list = %d", got)
	}
	if _, out := b.call(mainToken, "FinalizeCacheEntryUpload", map[string]string{"key": "big", "version": "v1", "size_bytes": "13"}); out["ok"] != true {
		t.Fatalf("finalize = %v", out)
	}
	if _, archive := b.restore(mainToken, "big", "v1"); archive != "one two three" {
		t.Errorf("archive = %q; want the blocks in the list's order", archive)
	}
}

func TestFinalizeChecksTheSize(t *testing.T) {
	b := newBox(t, 1<<30)
	_, out := b.call(mainToken, "CreateCacheEntry", map[string]string{"key": "k", "version": "v1"})
	b.put(out["signed_upload_url"].(string), "short")
	_, out = b.call(mainToken, "FinalizeCacheEntryUpload", map[string]string{"key": "k", "version": "v1", "size_bytes": "6"})
	if !reflect.DeepEqual(out, map[string]any{"ok": false, "message": "the upload holds 5 bytes, not 6"}) {
		t.Errorf("finalize = %v", out)
	}
	if key, _ := b.restore(mainToken, "k", "v1"); key != "" {
		t.Errorf("a failed finalize left an entry %q", key)
	}
}

// TestMatching follows GitHub's order: the key, then the newest entry it
// prefixes, then the restore keys, in the run's own scope before the others.
func TestMatching(t *testing.T) {
	b := newBox(t, 1<<30)
	b.save(mainToken, "deps-a", "v1", "main a")
	b.advance(time.Minute)
	b.save(mainToken, "deps-ab", "v1", "main ab")
	b.advance(time.Minute)
	b.save(mainToken, "tools-1", "v1", "main tools")
	b.advance(time.Minute)
	b.save(prToken, "tools-2", "v1", "pr tools")
	for _, c := range []struct {
		token, key string
		restore    []string
		want       string
	}{
		{mainToken, "deps-a", nil, "deps-a"},
		{mainToken, "deps-", nil, "deps-ab"},
		{mainToken, "deps-z", []string{"nope-", "deps-"}, "deps-ab"},
		{mainToken, "tools-", nil, "tools-1"},
		{prToken, "tools-", nil, "tools-2"},
		{prToken, "deps-a", nil, "deps-a"},
		{prToken, "x", []string{"tools-1"}, "tools-1"},
		{otherToken, "deps-a", []string{"deps-", "tools-"}, ""},
	} {
		if got, _ := b.restore(c.token, c.key, "v1", c.restore...); got != c.want {
			t.Errorf("restore(%s, %v) matched %q; want %q", c.key, c.restore, got, c.want)
		}
	}
}

func TestScopes(t *testing.T) {
	b := newBox(t, 1<<30)
	b.save(mainToken, "k", "v1", "from main")
	// A pull request writes to its own scope, so main's entry stays.
	b.save(prToken, "k", "v1", "from the pull request")
	if _, archive := b.restore(mainToken, "k", "v1"); archive != "from main" {
		t.Errorf("main restored %q", archive)
	}
	if _, archive := b.restore(prToken, "k", "v1"); archive != "from the pull request" {
		t.Errorf("the pull request restored %q", archive)
	}
	readOnly := runtimeToken("100", "refs/heads/main:1")
	status, out := b.call(readOnly, "CreateCacheEntry", map[string]string{"key": "k2", "version": "v1"})
	if status != http.StatusForbidden || out["msg"] != "cache write denied: the token has no writable scope" {
		t.Errorf("a read-only token's CreateCacheEntry = %d %v", status, out)
	}
	status, out = b.call(mainToken, "CreateCacheEntry", map[string]string{"key": "k", "version": "v1"})
	if status != 200 || !reflect.DeepEqual(out, map[string]any{"ok": false, "message": "an entry for this key and version exists or is being uploaded"}) {
		t.Errorf("CreateCacheEntry of an existing entry = %d %v", status, out)
	}
}

func TestTokens(t *testing.T) {
	b := newBox(t, 1<<30)
	other := mustKey()
	claims := func(change func(map[string]any)) map[string]any {
		c := map[string]any{"iss": Issuer, "nbf": testTime.Add(-time.Minute).Unix(), "exp": testTime.Add(time.Hour).Unix(),
			"repository_id": "100", "ac": `[{"Scope":"refs/heads/main","Permission":3}]`}
		change(c)
		return c
	}
	none := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","kid":"k1"}`)) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"`+Issuer+`","repository_id":"100"}`)) + "."
	for name, c := range map[string]struct{ token, msg string }{
		"no token":          {"", "no bearer token"},
		"another key":       {sign(other, "k1", claims(func(map[string]any) {})), "invalid runtime token: signature does not match"},
		"expired":           {sign(testKey, "k1", claims(func(c map[string]any) { c["exp"] = testTime.Add(-2 * time.Minute).Unix() })), "invalid runtime token: expired"},
		"another issuer":    {sign(testKey, "k1", claims(func(c map[string]any) { c["iss"] = "https://example.com" })), `invalid runtime token: issuer "https://example.com"`},
		"no scopes":         {sign(testKey, "k1", claims(func(c map[string]any) { delete(c, "ac") })), "invalid runtime token: no cache scopes"},
		"no repository":     {sign(testKey, "k1", claims(func(c map[string]any) { delete(c, "repository_id") })), "invalid runtime token: no repository_id"},
		"unsigned":          {none, `invalid runtime token: algorithm "none"`},
		"not a token":       {"abc", "invalid runtime token: not a JWT"},
		"an unknown key id": {sign(testKey, "k9", claims(func(map[string]any) {})), `invalid runtime token: unknown key "k9"`},
	} {
		status, out := b.call(c.token, "GetCacheEntryDownloadURL", map[string]any{"key": "k", "version": "v1"})
		if status != http.StatusUnauthorized || out["msg"] != c.msg {
			t.Errorf("%s: %d %v; want 401 %q", name, status, out, c.msg)
		}
	}
}

func TestKeysRefreshForANewKeyID(t *testing.T) {
	newKey := mustKey()
	keys := map[string]*rsa.PublicKey{"k1": &testKey.PublicKey}
	srv, fetches := jwks(t, keys)
	k := &Keys{URL: srv.URL, Issuer: Issuer, HTTP: http.DefaultClient}
	if _, err := k.Verify(t.Context(), mainToken, testTime); err != nil {
		t.Fatal(err)
	}
	keys["k2"] = &newKey.PublicKey
	token := sign(newKey, "k2", map[string]any{"iss": Issuer, "exp": testTime.Add(time.Hour).Unix(), "repository_id": "100", "ac": `[{"Scope":"refs/heads/main","Permission":1}]`})
	if _, err := k.Verify(t.Context(), token, testTime.Add(30*time.Second)); err == nil {
		t.Errorf("a new key id within a minute of the last read verified")
	}
	c, err := k.Verify(t.Context(), token, testTime.Add(2*time.Minute))
	want := Claims{Repo: "100", Scopes: []Scope{{"refs/heads/main", 1}}}
	if err != nil || !reflect.DeepEqual(c, want) || *fetches != 2 {
		t.Errorf("Verify after the new key appeared = %+v, %v, %d reads; want %+v and 2 reads", c, err, *fetches, want)
	}
}

func TestCapabilities(t *testing.T) {
	b := newBox(t, 1<<30)
	if got := b.put(b.srv.URL+"/cache/upload/guessed", "x"); got != http.StatusNotFound {
		t.Errorf("upload to an unknown URL = %d", got)
	}
	b.save(mainToken, "k", "v1", "secret archive")
	_, out := b.call(mainToken, "GetCacheEntryDownloadURL", map[string]any{"key": "k", "version": "v1"})
	download := out["signed_download_url"].(string)
	capability := strings.TrimPrefix(download, b.srv.URL+"/cache/download/")
	if len(capability) != 43 {
		t.Errorf("download capability %q is not 256 bits in base64", capability)
	}
	if code, _ := b.get(b.srv.URL + "/cache/download/" + capability[:42] + "A"); code != http.StatusNotFound {
		t.Errorf("download with a changed capability = %d", code)
	}
	b.advance(downloadFor + time.Second)
	if code, _ := b.get(download); code != http.StatusNotFound {
		t.Errorf("download after an hour = %d", code)
	}
	// An upload that stopped is dropped, and its key can be saved again.
	_, out = b.call(mainToken, "CreateCacheEntry", map[string]string{"key": "k2", "version": "v1"})
	stale := out["signed_upload_url"].(string)
	b.advance(uploadIdle + time.Second)
	b.store.Sweep(b.clock())
	if got := b.put(stale, "late"); got != http.StatusNotFound {
		t.Errorf("upload after an idle hour = %d", got)
	}
	b.save(mainToken, "k2", "v1", "again")
}

func TestEvictsLeastRecentlyUsed(t *testing.T) {
	b := newBox(t, 20)
	b.save(mainToken, "a", "v1", "aaaaaaaa")
	b.advance(time.Minute)
	b.save(mainToken, "b", "v1", "bbbbbbbb")
	b.advance(time.Minute)
	b.restore(mainToken, "a", "v1")
	b.advance(time.Minute)
	b.save(mainToken, "c", "v1", "cccccccc")
	var kept []string
	for _, k := range []string{"a", "b", "c"} {
		if got, _ := b.restore(mainToken, k, "v1"); got != "" {
			kept = append(kept, got)
		}
	}
	if !slices.Equal(kept, []string{"a", "c"}) {
		t.Errorf("kept %v; want a and c, since b was used least recently", kept)
	}
	if size, n := b.store.Size(); size != 16 || n != 2 {
		t.Errorf("Size = %d in %d; want 16 in 2", size, n)
	}
	status, out := b.call(mainToken, "CreateCacheEntry", map[string]string{"key": "huge", "version": "v1"})
	if b.put(out["signed_upload_url"].(string), strings.Repeat("h", 21)) != http.StatusBadRequest || status != 200 {
		t.Errorf("an archive larger than the cache was taken")
	}
}

// TestYieldsToTheDiskFloor: below the floor, the cache gives up its entries
// and takes no new ones, so jobs keep the disk.
func TestYieldsToTheDiskFloor(t *testing.T) {
	b := newBox(t, 1<<30)
	b.save(mainToken, "a", "v1", "aaaa")
	b.mu.Lock()
	b.free = 5 << 30
	b.mu.Unlock()
	b.store.Sweep(b.clock())
	if size, n := b.store.Size(); size != 0 || n != 0 {
		t.Errorf("below the floor, Size = %d in %d; want nothing", size, n)
	}
	_, out := b.call(mainToken, "CreateCacheEntry", map[string]string{"key": "b", "version": "v1"})
	if !reflect.DeepEqual(out, map[string]any{"ok": false, "message": "the box is short of disk; nothing is cached"}) {
		t.Errorf("CreateCacheEntry below the floor = %v", out)
	}
}

func TestReopenKeepsEntries(t *testing.T) {
	b := newBox(t, 1<<30)
	b.save(mainToken, "a", "v1", "kept")
	_, out := b.call(mainToken, "CreateCacheEntry", map[string]string{"key": "b", "version": "v1"})
	b.put(out["signed_upload_url"].(string), "unfinished")
	os.WriteFile(filepath.Join(b.store.Dir, "entries", "orphan"), []byte("x"), 0o600)

	again := &Store{Dir: b.store.Dir, Max: 1 << 30, Floor: 0, Free: b.store.Free}
	if err := again.Open(); err != nil {
		t.Fatal(err)
	}
	e, _, ok := again.Match(Claims{Repo: "100", Scopes: []Scope{{"refs/heads/main", 3}}}, "a", nil, "v1", testTime)
	if !ok || e.Key != "a" || e.Size != 4 {
		t.Errorf("after Open, Match = %+v %v", e, ok)
	}
	var files []string
	filepath.WalkDir(b.store.Dir, func(path string, d os.DirEntry, err error) error {
		if !d.IsDir() {
			files = append(files, strings.TrimPrefix(path, b.store.Dir+"/"))
		}
		return nil
	})
	if len(files) != 2 || !strings.HasPrefix(files[0], "entries/") || files[1] != files[0]+".json" {
		t.Errorf("files after Open = %v; want one archive and its description", files)
	}
}

func TestPassesOtherCallsToGitHub(t *testing.T) {
	b := newBox(t, 1<<30)
	req, _ := http.NewRequest(http.MethodPost, b.srv.URL+"/twirp/github.actions.results.api.v1.ArtifactService/CreateArtifact", strings.NewReader(`{"name":"dist"}`))
	req.Header.Set("Authorization", "Bearer job-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != `{"echo":"{\"name\":\"dist\"}"}` {
		t.Errorf("answer = %s", body)
	}
	code, github := b.get(b.srv.URL + UpstreamPath)
	if code != 200 || !strings.HasPrefix(github, "http://127.0.0.1:") {
		t.Errorf("GET %s = %d %q; want the upstream's URL", UpstreamPath, code, github)
	}
	if want := []string{"POST " + strings.TrimPrefix(github, "http://") + "/twirp/github.actions.results.api.v1.ArtifactService/CreateArtifact Bearer job-token"}; !slices.Equal(b.upstream, want) {
		t.Errorf("GitHub saw %v; want %v", b.upstream, want)
	}
}

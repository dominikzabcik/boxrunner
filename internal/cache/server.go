// Package cache serves GitHub Actions' cache service on the box, so the
// actions/cache steps of jobs here keep their archives on the box's disk
// instead of uploading them to GitHub. It speaks the cache service v2 that
// @actions/cache uses: three Twirp calls, uploads shaped like Azure Blob
// Storage's block uploads and plain downloads. Every other call to the
// results service, such as an artifact upload, it passes on to GitHub.
//
// Entries belong to the repository and ref scope named in the job's runtime
// token, which GitHub signs; upload and download URLs carry random
// capabilities, so one job cannot write or read another's archives.
package cache

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/layout"
)

// Upstream is GitHub's results service, which the runner names in
// ACTIONS_RESULTS_URL.
const Upstream = "https://results-receiver.actions.githubusercontent.com"

// UpstreamPath answers with the upstream the server passes calls on to. The
// job image's node wrapper reads it to check that the server is up and that
// the runner's ACTIONS_RESULTS_URL is the service it stands in for.
const UpstreamPath = "/_boxrunner/upstream"

const twirp = "/twirp/github.actions.results.api.v1.CacheService/"

// Paths of the upload and download URLs. The first segment stands for an
// Azure account: the Azure SDK reads a URL whose host is an IP address as
// /account/container/blob.
const (
	uploadPath   = "/cache/upload/"
	downloadPath = "/cache/download/"
)

// Addr is where the cache listens: the job network's gateway. The firewall
// drops what reaches its port from any other interface, so only containers on
// that network can reach it, not even the host.
func Addr(gateway netip.Addr) netip.AddrPort {
	return netip.AddrPortFrom(gateway, layout.CachePort)
}

// URL is the cache as jobs reach it.
func URL(addr netip.AddrPort) string { return "http://" + addr.String() }

type Server struct {
	Store *Store
	Keys  *Keys
	// Base is the server's URL as jobs reach it, such as http://172.18.0.1:5001.
	Base string
	// Upstream is where every call the cache does not answer goes.
	Upstream *url.URL
	Log      *slog.Logger
	Now      func() time.Time
}

// maxConns is the most connections the cache serves at once. A job's
// actions/cache opens at most 32 for one upload; more connections wait.
const maxConns = 256

// stall is how long an upload's body or a download may stop before the
// cache cuts it off.
var stall = 30 * time.Second

// Serve answers on l until ctx ends, sweeping the store every minute. Then
// it gives the calls in flight ten seconds; a longer upload is cut off, and
// its job saves no cache.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(limit(l, maxConns)) }()
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case err := <-served:
			return err
		case <-tick.C:
			s.Store.Sweep(s.Now())
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			if srv.Shutdown(shutdown) != nil {
				srv.Close()
			}
			<-served
			return nil
		}
	}
}

func (s *Server) Handler() http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite:  func(r *httputil.ProxyRequest) { r.SetURL(s.Upstream) },
		ErrorLog: slog.NewLogLogger(s.Log.Handler(), slog.LevelWarn),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+twirp+"CreateCacheEntry", s.create)
	mux.HandleFunc("POST "+twirp+"FinalizeCacheEntryUpload", s.finalize)
	mux.HandleFunc("POST "+twirp+"GetCacheEntryDownloadURL", s.lookup)
	mux.HandleFunc("PUT "+uploadPath+"{capability}", s.upload)
	mux.HandleFunc("GET "+downloadPath+"{capability}", s.download)
	mux.HandleFunc("GET "+UpstreamPath, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, s.Upstream.String())
	})
	mux.Handle("/", proxy)
	return mux
}

// claims verifies the bearer token and reads the Twirp request's JSON body.
func (s *Server) claims(w http.ResponseWriter, r *http.Request, body any) (Claims, bool) {
	bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		twirpError(w, http.StatusUnauthorized, "unauthenticated", "no bearer token")
		return Claims{}, false
	}
	c, err := s.Keys.Verify(r.Context(), bearer, s.Now())
	if err != nil {
		if errors.Is(err, ErrToken) {
			twirpError(w, http.StatusUnauthorized, "unauthenticated", err.Error())
		} else {
			twirpError(w, http.StatusServiceUnavailable, "unavailable", err.Error())
		}
		return Claims{}, false
	}
	http.NewResponseController(w).SetReadDeadline(time.Now().Add(stall))
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(body); err != nil {
		twirpError(w, http.StatusBadRequest, "malformed", err.Error())
		return Claims{}, false
	}
	return c, true
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key     string `json:"key"`
		Version string `json:"version"`
	}
	c, ok := s.claims(w, r, &req)
	if !ok {
		return
	}
	capability, err := s.Store.Reserve(c, req.Key, req.Version, s.Now())
	switch {
	case errors.Is(err, ErrDenied):
		twirpError(w, http.StatusForbidden, "permission_denied", err.Error())
	case err != nil:
		reply(w, map[string]any{"ok": false, "message": public(err)})
	default:
		reply(w, map[string]any{"ok": true, "signed_upload_url": s.Base + uploadPath + capability})
	}
}

func (s *Server) finalize(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key     string `json:"key"`
		Version string `json:"version"`
		Size    string `json:"size_bytes"`
	}
	c, ok := s.claims(w, r, &req)
	if !ok {
		return
	}
	size, err := strconv.ParseInt(req.Size, 10, 64)
	if err != nil {
		twirpError(w, http.StatusBadRequest, "malformed", "size_bytes: "+err.Error())
		return
	}
	e, err := s.Store.Finalize(c, req.Key, req.Version, size, s.Now())
	if err != nil {
		reply(w, map[string]any{"ok": false, "message": public(err)})
		return
	}
	s.Log.Info("cache: saved", "repo", e.Repo, "scope", e.Scope, "key", e.Key, "bytes", e.Size)
	// The toolkit parses the id as a number and only logs it.
	reply(w, map[string]any{"ok": true, "entry_id": strconv.FormatInt(e.Created.UnixNano(), 10)})
}

func (s *Server) lookup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key         string   `json:"key"`
		RestoreKeys []string `json:"restore_keys"`
		Version     string   `json:"version"`
	}
	c, ok := s.claims(w, r, &req)
	if !ok {
		return
	}
	if validKey(req.Key, req.Version) != nil || len(req.RestoreKeys) > 9 || slicesContainsEmpty(req.RestoreKeys) {
		twirpError(w, http.StatusBadRequest, "invalid_argument", "a key, up to nine restore keys and a version are needed")
		return
	}
	e, capability, ok := s.Store.Match(c, req.Key, req.RestoreKeys, req.Version, s.Now())
	if !ok {
		reply(w, map[string]any{"ok": false})
		return
	}
	reply(w, map[string]any{"ok": true, "signed_download_url": s.Base + downloadPath + capability, "matched_key": e.Key})
}

func slicesContainsEmpty(keys []string) bool {
	for _, k := range keys {
		if k == "" || len(k) > maxKey {
			return true
		}
	}
	return false
}

// upload takes Azure Blob Storage's Put Blob, Put Block and Put Block List.
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	capability, q, now := r.PathValue("capability"), r.URL.Query(), s.Now()
	// The deadline stays after the handler, so the server's read of what is
	// left of a stalled body also stops; it sets its own for the next request.
	body := deadline{r.Body, http.NewResponseController(w).SetReadDeadline}
	var err error
	switch q.Get("comp") {
	case "":
		err = s.Store.Put(capability, "", body, r.ContentLength, now)
	case "block":
		if q.Get("blockid") == "" {
			err = errors.New("no blockid")
		} else {
			err = s.Store.Put(capability, q.Get("blockid"), body, r.ContentLength, now)
		}
	case "blocklist":
		var list struct {
			Blocks []string `xml:",any"`
		}
		if err = xml.NewDecoder(deadline{http.MaxBytesReader(w, r.Body, 4<<20), body.set}).Decode(&list); err == nil {
			err = s.Store.Commit(capability, list.Blocks, now)
		}
	default:
		err = fmt.Errorf("comp=%s is not supported", q.Get("comp"))
	}
	// Some clients fail on an answer without a request id, as Azure always sends one.
	w.Header().Set("x-ms-request-id", token())
	switch {
	case errors.Is(err, ErrUnknown):
		http.Error(w, err.Error(), http.StatusNotFound)
	case err != nil:
		http.Error(w, public(err), http.StatusBadRequest)
	default:
		w.WriteHeader(http.StatusCreated)
	}
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	f, e, err := s.Store.Download(r.PathValue("capability"), s.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("x-ms-blob-type", "BlockBlob")
	// The server keeps a write deadline for the connection's next request,
	// so a download is its connection's last.
	w.Header().Set("Connection", "close")
	http.ServeContent(w, r, "", e.Created, struct {
		deadline
		io.Seeker
	}{deadline{f, http.NewResponseController(w).SetWriteDeadline}, f})
}

// deadline gives each read from r, and on a download the write of what it
// read, stall to finish.
type deadline struct {
	r   io.Reader
	set func(time.Time) error
}

func (d deadline) Read(p []byte) (int, error) {
	d.set(time.Now().Add(stall))
	return d.r.Read(p)
}

// public is err as a job may read it: without the box's paths.
func public(err error) string {
	var path *fs.PathError
	var link *os.LinkError
	switch {
	case errors.As(err, &path):
		return path.Op + ": " + path.Err.Error()
	case errors.As(err, &link):
		return link.Op + ": " + link.Err.Error()
	}
	return err.Error()
}

// limit makes l accept at most n connections at once.
func limit(l net.Listener, n int) net.Listener {
	return &limited{Listener: l, slots: make(chan struct{}, n), closed: make(chan struct{})}
}

type limited struct {
	net.Listener
	slots  chan struct{}
	closed chan struct{}
	once   sync.Once
}

func (l *limited) Accept() (net.Conn, error) {
	select {
	case l.slots <- struct{}{}:
	case <-l.closed:
		return nil, net.ErrClosed
	}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	return &slot{Conn: c, free: func() { <-l.slots }}, nil
}

func (l *limited) Close() error {
	l.once.Do(func() { close(l.closed) })
	return l.Listener.Close()
}

type slot struct {
	net.Conn
	once sync.Once
	free func()
}

func (c *slot) Close() error {
	c.once.Do(c.free)
	return c.Conn.Close()
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func twirpError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"code": code, "msg": msg})
}

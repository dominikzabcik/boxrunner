package cache

import (
	"cmp"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// uploadIdle is how long an upload may wait between blocks.
	uploadIdle = time.Hour
	// downloadFor is how long a download URL works.
	downloadFor = time.Hour
	maxKey      = 512
)

var (
	// ErrExists means an entry or an upload for the key, version and scope
	// already exists; entries never change once written.
	ErrExists = errors.New("an entry for this key and version exists or is being uploaded")
	// ErrDenied means the token's scopes do not allow the call.
	ErrDenied = errors.New("cache write denied: the token has no writable scope")
	// ErrFull means the box has no room for a new entry above its disk floor.
	ErrFull = errors.New("the box is short of disk; nothing is cached")
	// ErrUnknown means a capability expired or never existed.
	ErrUnknown = errors.New("unknown or expired URL")
)

// Entry is one cached archive. Its scope is the ref that wrote it.
type Entry struct {
	Repo    string    `json:"repo"`
	Scope   string    `json:"scope"`
	Key     string    `json:"key"`
	Version string    `json:"version"`
	Size    int64     `json:"size"`
	Created time.Time `json:"created"`

	id string
	// used is when a job last restored or wrote the entry, kept as the
	// archive's modification time.
	used time.Time
}

type upload struct {
	Entry
	dir  string
	last time.Time
}

type download struct {
	id      string
	expires time.Time
}

// Store keeps entries in Dir, each an archive named by a random id with its
// Entry beside it as JSON, and holds the uploads in progress and the download
// URLs it handed out. It keeps the archives within Max bytes and gives up the
// least recently used ones while Free reports less than Floor.
type Store struct {
	Dir   string
	Max   int64
	Floor int64
	Free  func() (int64, error)

	mu        sync.Mutex
	entries   map[string]*Entry
	size      int64
	uploads   map[string]*upload
	downloads map[string]download
}

func (s *Store) entriesDir() string { return filepath.Join(s.Dir, "entries") }
func (s *Store) uploadsDir() string { return filepath.Join(s.Dir, "uploads") }

// Open reads the entries in Dir and drops what an interrupted run left:
// uploads, and archives or descriptions missing their other half.
func (s *Store) Open() error {
	s.entries, s.uploads, s.downloads = map[string]*Entry{}, map[string]*upload{}, map[string]download{}
	if err := os.RemoveAll(s.uploadsDir()); err != nil {
		return err
	}
	for _, dir := range []string{s.entriesDir(), s.uploadsDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	files, err := os.ReadDir(s.entriesDir())
	if err != nil {
		return err
	}
	for _, f := range files {
		id, isMeta := strings.CutSuffix(f.Name(), ".json")
		archive := filepath.Join(s.entriesDir(), id)
		info, err := os.Stat(archive)
		var e Entry
		if isMeta && err == nil {
			b, err := os.ReadFile(filepath.Join(s.entriesDir(), f.Name()))
			if err == nil && json.Unmarshal(b, &e) == nil && e.Size == info.Size() {
				e.id, e.used = id, info.ModTime()
				s.entries[id] = &e
				s.size += e.Size
				continue
			}
		}
		if _, err := os.Stat(archive + ".json"); isMeta || err != nil {
			os.Remove(archive)
			os.Remove(archive + ".json")
		}
	}
	return nil
}

// Reserve starts an upload into c's writable scope and returns its capability.
func (s *Store) Reserve(c Claims, key, version string, now time.Time) (string, error) {
	scope, ok := writable(c)
	if !ok {
		return "", ErrDenied
	}
	if err := validKey(key, version); err != nil {
		return "", err
	}
	want := Entry{Repo: c.Repo, Scope: scope, Key: key, Version: version}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if same(*e, want) {
			return "", ErrExists
		}
	}
	for _, u := range s.uploads {
		if same(u.Entry, want) {
			return "", ErrExists
		}
	}
	s.evict()
	if free, err := s.Free(); err != nil || free < s.Floor {
		return "", ErrFull
	}
	capability := token()
	dir := filepath.Join(s.uploadsDir(), capability)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", err
	}
	s.uploads[capability] = &upload{Entry: want, dir: dir, last: now}
	return capability, nil
}

// Put writes an upload's archive at once, or with block set, one block of
// it. Each write may hold at most Max bytes.
func (s *Store) Put(capability, block string, body io.Reader, now time.Time) error {
	name := "archive"
	if block != "" {
		if len(block) > 128 {
			return fmt.Errorf("block id longer than 128 characters")
		}
		name = "block-" + hex.EncodeToString([]byte(block))
	}
	u, err := s.touch(capability, now)
	if err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(u.dir, name))
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(body, s.Max+1))
	if err == nil && n > s.Max {
		err = fmt.Errorf("the archive is larger than the cache's %d bytes", s.Max)
	}
	return errors.Join(err, f.Close())
}

// Commit joins an upload's blocks in the order given into its archive.
func (s *Store) Commit(capability string, blocks []string, now time.Time) error {
	u, err := s.touch(capability, now)
	if err != nil {
		return err
	}
	archive, err := os.Create(filepath.Join(u.dir, "archive"))
	if err != nil {
		return err
	}
	var total int64
	for _, b := range blocks {
		path := filepath.Join(u.dir, "block-"+hex.EncodeToString([]byte(b)))
		f, err := os.Open(path)
		if err != nil {
			archive.Close()
			return fmt.Errorf("block %q was not uploaded", b)
		}
		n, err := io.Copy(archive, f)
		f.Close()
		if total += n; err == nil && total > s.Max {
			err = fmt.Errorf("the archive is larger than the cache's %d bytes", s.Max)
		}
		if err != nil {
			archive.Close()
			return err
		}
	}
	return archive.Close()
}

func (s *Store) touch(capability string, now time.Time) (*upload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.uploads[capability]
	if !ok {
		return nil, ErrUnknown
	}
	u.last = now
	return u, nil
}

// Finalize turns c's upload for key and version into an entry once its
// archive has size bytes, then evicts what no longer fits.
func (s *Store) Finalize(c Claims, key, version string, size int64, now time.Time) (Entry, error) {
	scope, ok := writable(c)
	if !ok {
		return Entry{}, ErrDenied
	}
	want := Entry{Repo: c.Repo, Scope: scope, Key: key, Version: version}
	s.mu.Lock()
	defer s.mu.Unlock()
	var capability string
	for k, u := range s.uploads {
		if same(u.Entry, want) {
			capability = k
		}
	}
	u, ok := s.uploads[capability]
	if !ok {
		return Entry{}, ErrUnknown
	}
	delete(s.uploads, capability)
	defer os.RemoveAll(u.dir)
	info, err := os.Stat(filepath.Join(u.dir, "archive"))
	if err != nil || info.Size() != size || size > s.Max {
		return Entry{}, fmt.Errorf("the upload holds %d bytes, not %d", sizeOf(info), size)
	}
	e := u.Entry
	e.Size, e.Created, e.used, e.id = size, now.UTC(), now, token()
	meta, err := json.Marshal(e)
	if err != nil {
		return Entry{}, err
	}
	archive := filepath.Join(s.entriesDir(), e.id)
	if err := os.Rename(filepath.Join(u.dir, "archive"), archive); err != nil {
		return Entry{}, err
	}
	if err := writeFile(archive+".json", meta); err != nil {
		os.Remove(archive)
		return Entry{}, err
	}
	os.Chtimes(archive, now, now)
	s.entries[e.id] = &e
	s.size += e.Size
	s.evict()
	return e, nil
}

func sizeOf(info os.FileInfo) int64 {
	if info == nil {
		return 0
	}
	return info.Size()
}

// Match finds the entry a restore gets, as GitHub's cache does: in each
// readable scope, the run's own first, the key itself, then the newest entry
// whose key starts with it, then the newest that starts with each restore
// key in turn. It returns the entry and a download capability.
func (s *Store) Match(c Claims, key string, restore []string, version string, now time.Time) (Entry, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, scope := range readable(c) {
		var candidates []*Entry
		for _, e := range s.entries {
			if e.Repo == c.Repo && e.Scope == scope && e.Version == version {
				candidates = append(candidates, e)
			}
		}
		// Newest first, so the first prefix match is the newest.
		slices.SortFunc(candidates, func(a, b *Entry) int { return cmp.Or(b.Created.Compare(a.Created), cmp.Compare(a.id, b.id)) })
		find := func(match func(*Entry) bool) *Entry {
			if i := slices.IndexFunc(candidates, match); i >= 0 {
				return candidates[i]
			}
			return nil
		}
		hit := find(func(e *Entry) bool { return e.Key == key })
		for _, prefix := range append([]string{key}, restore...) {
			if hit == nil {
				hit = find(func(e *Entry) bool { return strings.HasPrefix(e.Key, prefix) })
			}
		}
		if hit != nil {
			hit.used = now
			os.Chtimes(filepath.Join(s.entriesDir(), hit.id), now, now)
			capability := token()
			s.downloads[capability] = download{id: hit.id, expires: now.Add(downloadFor)}
			return *hit, capability, true
		}
	}
	return Entry{}, "", false
}

// Download opens the archive a download capability names.
func (s *Store) Download(capability string, now time.Time) (*os.File, Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.downloads[capability]
	e, found := s.entries[d.id]
	if !ok || !found || now.After(d.expires) {
		return nil, Entry{}, ErrUnknown
	}
	f, err := os.Open(filepath.Join(s.entriesDir(), d.id))
	return f, *e, err
}

// Sweep drops idle uploads and expired download URLs, then evicts.
func (s *Store) Sweep(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, u := range s.uploads {
		if now.Sub(u.last) > uploadIdle {
			delete(s.uploads, k)
			os.RemoveAll(u.dir)
		}
	}
	for k, d := range s.downloads {
		if now.After(d.expires) {
			delete(s.downloads, k)
		}
	}
	s.evict()
}

// Size is what the entries take, and how many there are.
func (s *Store) Size() (int64, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.size, len(s.entries)
}

// evict removes the least recently used entries while they exceed Max or
// the disk has less than Floor free. The caller holds mu.
func (s *Store) evict() {
	for len(s.entries) > 0 {
		if s.size <= s.Max {
			if free, err := s.Free(); err != nil || free >= s.Floor {
				return
			}
		}
		var oldest *Entry
		for _, e := range s.entries {
			if oldest == nil || e.used.Before(oldest.used) {
				oldest = e
			}
		}
		archive := filepath.Join(s.entriesDir(), oldest.id)
		os.Remove(archive + ".json")
		os.Remove(archive)
		delete(s.entries, oldest.id)
		s.size -= oldest.Size
	}
}

func same(a, b Entry) bool {
	return a.Repo == b.Repo && a.Scope == b.Scope && a.Key == b.Key && a.Version == b.Version
}

// writable is the scope a job writes to: the first its token may write.
func writable(c Claims) (string, bool) {
	for _, s := range c.Scopes {
		if s.Permission&Write != 0 {
			return s.Name, true
		}
	}
	return "", false
}

// readable are the scopes a job may restore from, the writable one first.
// A token for cache-mode write-only can write a scope it cannot read.
func readable(c Claims) []string {
	var scopes []string
	for _, s := range c.Scopes {
		if s.Permission&Read == 0 || slices.Contains(scopes, s.Name) {
			continue
		}
		if w, _ := writable(c); s.Name == w {
			scopes = slices.Insert(scopes, 0, s.Name)
		} else {
			scopes = append(scopes, s.Name)
		}
	}
	return scopes
}

func validKey(key, version string) error {
	if key == "" || version == "" || len(key) > maxKey || len(version) > maxKey {
		return fmt.Errorf("key and version must have 1 to %d characters", maxKey)
	}
	return nil
}

// token is a capability: 256 random bits, URL-safe.
func token() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func writeFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

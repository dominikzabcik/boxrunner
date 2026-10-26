package cache

import (
	"cmp"
	"crypto/rand"
	"crypto/sha256"
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
	uploadIdle = 10 * time.Minute
	// downloadFor is how long a download URL works.
	downloadFor = time.Hour
	maxKey      = 512
	// maxBlocks is the most blocks an upload holds. actions/cache sends
	// blocks of 32 MiB, so 4,000 make 125 GiB; each block is a file.
	maxBlocks = 4000
	// maxUploads and maxRepoUploads are the most uploads in progress, in all
	// and for one repository. actions/cache keeps one open per save step.
	maxUploads     = 64
	maxRepoUploads = 16
	// maxEntries is the most entries a repository keeps, so a lookup stays
	// quick.
	maxEntries = 10000
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
	// ErrNoRoom means a write would take the cache past Max, or the disk
	// below Floor, even with every entry it may give up gone.
	ErrNoRoom = errors.New("the cache has no room for this archive")

	errWriting = errors.New("the upload is still being written")
	errJoining = errors.New("the upload's blocks are being joined")
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
	// files are the sizes of the upload's finished files: its archive and
	// its blocks.
	files map[string]int64
	// bytes is what the upload takes on disk, its writes in progress
	// included.
	bytes int64
	// writes counts the writes in progress, or is -1 while the blocks are
	// joined into the archive.
	writes int
}

type download struct {
	id      string
	expires time.Time
}

// Store keeps entries in Dir, each an archive named by a random id with its
// Entry beside it as JSON, and holds the uploads in progress and the download
// URLs it handed out. Entries and uploads together stay within Max bytes and
// above Floor free bytes: to make room, the store gives up the least recently
// used entry of the repository that holds the most, and a write that would not
// fit even so is refused before it gives up any.
type Store struct {
	Dir   string
	Max   int64
	Floor int64
	Free  func() (int64, error)

	mu      sync.Mutex
	entries map[string]*Entry
	size    int64
	uploads map[string]*upload
	// open is what the uploads take.
	open      int64
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
	repoUploads := 0
	for _, u := range s.uploads {
		if same(u.Entry, want) {
			return "", ErrExists
		}
		if u.Repo == c.Repo {
			repoUploads++
		}
	}
	switch {
	case len(s.uploads) >= maxUploads:
		return "", fmt.Errorf("the cache has %d uploads in progress, the most it takes", maxUploads)
	case repoUploads >= maxRepoUploads:
		return "", fmt.Errorf("this repository has %d uploads in progress, the most the cache takes", maxRepoUploads)
	case !s.room("", 0, false):
		return "", ErrFull
	}
	capability := token()
	dir := filepath.Join(s.uploadsDir(), capability)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", err
	}
	s.uploads[capability] = &upload{Entry: want, dir: dir, last: now, files: map[string]int64{}}
	return capability, nil
}

// Put writes an upload's archive at once, or with block set, one block of
// it. It writes into a new file and puts it in place when the body ends, so
// a write that fails leaves the upload as it was. size is the body's
// length, or -1 when unknown. A body of known length reserves its size
// before it is read, so one that cannot fit is refused without giving up an
// entry; a body of unknown length takes room as it comes, and takes entries
// from other repositories only until its writer holds a share of the cache.
func (s *Store) Put(capability, block string, body io.Reader, size int64, now time.Time) error {
	name := "archive"
	if block != "" {
		if len(block) > 128 {
			return fmt.Errorf("block id longer than 128 characters")
		}
		name = blockFile(block)
	}
	u, err := s.begin(capability, name, size, now)
	if err != nil {
		return err
	}
	w := &budget{s: s, u: u, held: max(size, 0), limited: size <= 0}
	tmp, err := os.CreateTemp(u.dir, "write-")
	if err == nil {
		w.f = tmp
		_, err = io.Copy(w, body)
		err = errors.Join(err, tmp.Close())
		if err == nil {
			err = os.Rename(tmp.Name(), filepath.Join(u.dir, name))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u.writes--
	if err != nil {
		if tmp != nil {
			os.Remove(tmp.Name())
		}
		s.release(u, w.held)
		return err
	}
	// The write gives back what it reserved and did not use, and the file it
	// replaces.
	s.release(u, u.files[name]+w.held-w.n)
	u.files[name] = w.n
	return nil
}

// begin counts a write of the file name, of size bytes, into an upload, and
// reserves the size when it is known.
func (s *Store) begin(capability, name string, size int64, now time.Time) (*upload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.uploads[capability]
	if !ok {
		return nil, ErrUnknown
	}
	if u.writes < 0 {
		return nil, errJoining
	}
	if _, ok := u.files[name]; !ok && len(u.files)+u.writes >= maxBlocks {
		return nil, fmt.Errorf("an upload holds at most %d blocks", maxBlocks)
	}
	if size > 0 {
		if err := s.take(u, size, false); err != nil {
			return nil, err
		}
	}
	u.writes++
	u.last = now
	return u, nil
}

// budget writes to f only what the store has room for, and counts it. held is
// what the write has taken from the store: the length it reserved, or the
// bytes written when they pass it. limited is set for a body of unknown
// length.
type budget struct {
	s       *Store
	u       *upload
	f       *os.File
	n       int64
	held    int64
	limited bool
}

func (w *budget) Write(p []byte) (int, error) {
	if more := w.n + int64(len(p)) - w.held; more > 0 {
		if err := w.s.grow(w.u, more, w.limited); err != nil {
			return 0, err
		}
		w.held += more
	}
	w.n += int64(len(p))
	return w.f.Write(p)
}

func (s *Store) grow(u *upload, n int64, limited bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.take(u, n, limited)
}

// take counts n more bytes in u once the store has made room for them. The
// caller holds mu.
func (s *Store) take(u *upload, n int64, limited bool) error {
	if !s.room(u.Repo, n, limited) {
		return ErrNoRoom
	}
	u.bytes += n
	s.open += n
	return nil
}

// release gives up n bytes of an upload. The caller holds mu.
func (s *Store) release(u *upload, n int64) {
	u.bytes -= n
	s.open -= n
}

// Commit joins an upload's blocks in the order given into its archive and
// deletes each block once it is in, so the upload takes at most one block
// more than its size, and that block counts toward Max. Blocks the list
// leaves out are deleted. A list that fails while the blocks are joined ends
// the upload.
func (s *Store) Commit(capability string, blocks []string, now time.Time) error {
	s.mu.Lock()
	u, ok := s.uploads[capability]
	if !ok {
		s.mu.Unlock()
		return ErrUnknown
	}
	names := make([]string, len(blocks))
	sizes := make([]int64, len(blocks))
	for i, b := range blocks {
		names[i] = blockFile(b)
		var ok bool
		if sizes[i], ok = u.files[names[i]]; !ok || slices.Contains(names[:i], names[i]) {
			s.mu.Unlock()
			return fmt.Errorf("block %q was not uploaded or is listed twice", b)
		}
	}
	if u.writes != 0 {
		s.mu.Unlock()
		return errWriting
	}
	u.writes, u.last = -1, now
	s.mu.Unlock()

	size, err := s.join(u, names, sizes)
	s.mu.Lock()
	defer s.mu.Unlock()
	u.writes = 0
	if err != nil {
		s.drop(capability)
		return err
	}
	for name := range u.files {
		if name != "archive" && !slices.Contains(names, name) {
			os.Remove(filepath.Join(u.dir, name))
		}
	}
	s.release(u, u.bytes-size)
	u.files = map[string]int64{"archive": size}
	return nil
}

// join appends the blocks names, of sizes bytes, into u's archive, deleting
// each once it is in, and returns the archive's size.
func (s *Store) join(u *upload, names []string, sizes []int64) (int64, error) {
	archive, err := os.CreateTemp(u.dir, "write-")
	if err != nil {
		return 0, err
	}
	defer archive.Close()
	var size int64
	for i, name := range names {
		if err := s.grow(u, sizes[i], false); err != nil {
			return 0, err
		}
		f, err := os.Open(filepath.Join(u.dir, name))
		if err != nil {
			return 0, err
		}
		_, err = io.Copy(archive, io.LimitReader(f, sizes[i]))
		f.Close()
		if err == nil {
			err = os.Remove(f.Name())
		}
		if err != nil {
			return 0, err
		}
		s.mu.Lock()
		s.release(u, sizes[i])
		s.mu.Unlock()
		size += sizes[i]
	}
	if err := archive.Close(); err != nil {
		return 0, err
	}
	return size, os.Rename(archive.Name(), filepath.Join(u.dir, "archive"))
}

// drop ends an upload and deletes its files. The caller holds mu.
func (s *Store) drop(capability string) {
	u := s.uploads[capability]
	delete(s.uploads, capability)
	os.RemoveAll(u.dir)
	s.release(u, u.bytes)
}

func blockFile(id string) string {
	sum := sha256.Sum256([]byte(id))
	return "block-" + hex.EncodeToString(sum[:])
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
	if u.writes != 0 {
		return Entry{}, errWriting
	}
	defer s.drop(capability)
	if held, ok := u.files["archive"]; !ok || held != size {
		return Entry{}, fmt.Errorf("the upload holds %d bytes, not %d", held, size)
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
	s.release(u, size)
	s.size += size
	if err := writeFile(archive+".json", meta); err != nil {
		os.Remove(archive)
		s.size -= size
		return Entry{}, err
	}
	os.Chtimes(archive, now, now)
	s.entries[e.id] = &e
	if s.count(e.Repo) > maxEntries {
		s.remove(s.oldest(func(o *Entry) bool { return o.Repo == e.Repo }))
	}
	s.room("", 0, false)
	return e, nil
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

// Sweep drops idle uploads and expired download URLs, then makes room.
// While the disk stays below Floor, it also drops every upload between
// writes.
func (s *Store) Sweep(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, d := range s.downloads {
		if now.After(d.expires) {
			delete(s.downloads, k)
		}
	}
	short := !s.room("", 0, false)
	for k, u := range s.uploads {
		if u.writes == 0 && (short || now.Sub(u.last) > uploadIdle) {
			s.drop(k)
		}
	}
}

// Size is what the entries take, and how many there are.
func (s *Store) Size() (int64, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.size, len(s.entries)
}

// room gives up entries until need more bytes for repo fit within Max and
// above Floor, and says whether they do. Each entry it gives up is the least
// recently used of the repository that holds the most, counting uploads and
// need, with repo first among equals, so a repository makes room from its own
// entries before those of a repository that holds less. It plans the entries
// first and gives up none when they would not be enough: a write whose
// repository holds the most and has too few entries of its own, or that could
// not fit with every entry gone, removes nothing. With limited set, once repo
// holds its share of Max, an equal part for each repository that holds
// anything, the plan takes only repo's own entries. For repo "", which asks
// for nothing, it gives up as many as it can. The caller holds mu.
func (s *Store) room(repo string, need int64, limited bool) bool {
	free, err := s.Free()
	if err != nil {
		return false
	}
	evict, ok := s.plan(repo, need, limited, free)
	if ok || repo == "" {
		for _, e := range evict {
			s.remove(e)
		}
	}
	return ok
}

// plan lists the entries room would give up for repo, in order, when free
// bytes are free, and says whether they make room. The caller holds mu.
func (s *Store) plan(repo string, need int64, limited bool, free int64) ([]*Entry, bool) {
	fit := func(freed int64) bool {
		return s.size-freed+s.open+need <= s.Max && free+freed-need >= s.Floor
	}
	if fit(0) {
		return nil, true
	}
	// With every entry gone, still too much: nothing is worth giving up.
	if repo != "" && !fit(s.size) {
		return nil, false
	}
	held := map[string]int64{repo: need}
	queue := map[string][]*Entry{}
	for _, e := range s.entries {
		held[e.Repo] += e.Size
		queue[e.Repo] = append(queue[e.Repo], e)
	}
	for _, u := range s.uploads {
		held[u.Repo] += u.bytes
	}
	repos := make([]string, 0, len(queue))
	for r, q := range queue {
		repos = append(repos, r)
		slices.SortFunc(q, func(a, b *Entry) int { return cmp.Or(a.used.Compare(b.used), cmp.Compare(a.id, b.id)) })
	}
	slices.Sort(repos)
	holders := int64(1)
	for r, n := range held {
		if r != repo && n > 0 {
			holders++
		}
	}
	share := s.Max / holders

	var evict []*Entry
	var freed int64
	for !fit(freed) {
		top := repo
		if !limited || held[repo]-need < share {
			for _, r := range repos {
				if len(queue[r]) > 0 && held[r] > held[top] {
					top = r
				}
			}
		}
		if len(queue[top]) == 0 {
			return evict, false
		}
		e := queue[top][0]
		queue[top] = queue[top][1:]
		held[top] -= e.Size
		freed += e.Size
		evict = append(evict, e)
	}
	return evict, true
}

// oldest is the least recently used entry that keep is true for. The caller
// holds mu.
func (s *Store) oldest(keep func(*Entry) bool) *Entry {
	var oldest *Entry
	for _, e := range s.entries {
		if keep(e) && (oldest == nil || e.used.Before(oldest.used)) {
			oldest = e
		}
	}
	return oldest
}

// count is how many entries repo has. The caller holds mu.
func (s *Store) count(repo string) int {
	n := 0
	for _, e := range s.entries {
		if e.Repo == repo {
			n++
		}
	}
	return n
}

// remove deletes an entry. The caller holds mu.
func (s *Store) remove(e *Entry) {
	archive := filepath.Join(s.entriesDir(), e.id)
	os.Remove(archive + ".json")
	os.Remove(archive)
	delete(s.entries, e.id)
	s.size -= e.Size
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

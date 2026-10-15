package cache

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// reserve starts an upload as actions/cache does and returns its URL.
func (b *box) reserve(token, key string) string {
	b.t.Helper()
	_, out := b.call(token, "CreateCacheEntry", map[string]string{"key": key, "version": "v1"})
	u, ok := out["signed_upload_url"].(string)
	if !ok {
		b.t.Fatalf("CreateCacheEntry(%s) = %v", key, out)
	}
	return u
}

func (b *box) setFree(free int64) {
	b.mu.Lock()
	b.free = free
	b.mu.Unlock()
}

// onDisk is what the uploads in progress take on disk.
func (b *box) onDisk() int64 {
	var n int64
	filepath.Walk(filepath.Join(b.store.Dir, "uploads"), func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			n += info.Size()
		}
		return nil
	})
	return n
}

func TestBlocksCountTowardTheCache(t *testing.T) {
	b := newBox(t, 1000)
	u := b.reserve(mainToken, "k")
	var got []int
	for i := range 3 {
		got = append(got, b.put(fmt.Sprintf("%s?comp=block&blockid=b%d", u, i), strings.Repeat("x", 400)))
	}
	if want := []int{201, 201, 400}; !reflect.DeepEqual(got, want) || b.onDisk() != 800 {
		t.Errorf("three blocks of 400 bytes in a cache of 1000 = %v with %d bytes on disk; want %v and 800", got, b.onDisk(), want)
	}
}

func TestOpenUploadsCountTowardTheCache(t *testing.T) {
	b := newBox(t, 1000)
	first, second := b.reserve(mainToken, "a"), b.reserve(mainToken, "b")
	if got := []int{b.put(first, strings.Repeat("a", 600)), b.put(second, strings.Repeat("b", 600))}; !reflect.DeepEqual(got, []int{201, 400}) {
		t.Errorf("two uploads of 600 bytes in a cache of 1000 = %v; want 201 and 400", got)
	}
	if b.onDisk() != 600 {
		t.Errorf("uploads take %d bytes; want 600", b.onDisk())
	}
}

func TestWritesStopAtTheDiskFloor(t *testing.T) {
	b := newBox(t, 1<<30)
	u := b.reserve(mainToken, "k")
	b.setFree(5 << 30)
	if got := b.put(u+"?comp=block&blockid=b0", "x"); got != http.StatusBadRequest || b.onDisk() != 0 {
		t.Errorf("a block below the disk floor = %d with %d bytes on disk; want 400 and none", got, b.onDisk())
	}
}

// TestSweepDropsUploads: an upload left idle for ten minutes is dropped, and
// below the disk floor so is every upload between writes.
func TestSweepDropsUploads(t *testing.T) {
	b := newBox(t, 1<<30)
	idle := b.reserve(mainToken, "idle")
	b.advance(10*time.Minute + time.Second)
	b.store.Sweep(b.clock())
	if got := b.put(idle, "late"); got != http.StatusNotFound {
		t.Errorf("an upload after ten idle minutes = %d; want 404", got)
	}
	open := b.reserve(mainToken, "open")
	b.put(open+"?comp=block&blockid=b0", "part")
	b.setFree(5 << 30)
	b.store.Sweep(b.clock())
	if got := b.put(open+"?comp=block&blockid=b1", "more"); got != http.StatusNotFound || b.onDisk() != 0 {
		t.Errorf("below the floor, an open upload took a block (%d) and %d bytes stayed on disk", got, b.onDisk())
	}
}

func TestCommitDoesNotCopyTheBlocks(t *testing.T) {
	b := newBox(t, 1000)
	u := b.reserve(mainToken, "k")
	b.put(u+"?comp=block&blockid=b0", strings.Repeat("x", 300))
	b.put(u+"?comp=block&blockid=b1", strings.Repeat("y", 300))
	b.put(u+"?comp=block&blockid=unused", strings.Repeat("z", 100))
	if got := b.put(u+"?comp=blocklist", "<BlockList><Latest>b0</Latest><Latest>b1</Latest></BlockList>"); got != http.StatusCreated {
		t.Fatalf("block list = %d", got)
	}
	if b.onDisk() != 600 {
		t.Errorf("after the block list, the upload takes %d bytes; want 600, the archive alone", b.onDisk())
	}
	if _, out := b.call(mainToken, "FinalizeCacheEntryUpload", map[string]string{"key": "k", "version": "v1", "size_bytes": "600"}); out["ok"] != true {
		t.Errorf("finalize = %v", out)
	}
}

func TestBlocksPerUpload(t *testing.T) {
	b := newBox(t, 1<<30)
	u := b.reserve(mainToken, "k")
	for i := range 4000 {
		if got := b.put(fmt.Sprintf("%s?comp=block&blockid=%05d", u, i), "x"); got != http.StatusCreated {
			t.Fatalf("block %d = %d", i, got)
		}
	}
	if got := b.put(u+"?comp=block&blockid=04001", "x"); got != http.StatusBadRequest {
		t.Errorf("block 4001 = %d; want 400", got)
	}
	if got := b.put(u+"?comp=block&blockid=00007", "again"); got != http.StatusCreated {
		t.Errorf("writing block 7 again = %d; want 201", got)
	}
}

func TestUploadsInProgress(t *testing.T) {
	b := newBox(t, 1<<30)
	for i := range 16 {
		b.reserve(mainToken, fmt.Sprint("main-", i))
	}
	_, out := b.call(mainToken, "CreateCacheEntry", map[string]string{"key": "main-16", "version": "v1"})
	if want := map[string]any{"ok": false, "message": "this repository has 16 uploads in progress, the most the cache takes"}; !reflect.DeepEqual(out, want) {
		t.Errorf("a 17th upload of one repository = %v; want %v", out, want)
	}
	for r := range 3 {
		token := runtimeToken(fmt.Sprint(300+r), "refs/heads/main:3")
		for i := range 16 {
			b.reserve(token, fmt.Sprint("k", i))
		}
	}
	_, out = b.call(otherToken, "CreateCacheEntry", map[string]string{"key": "k", "version": "v1"})
	if want := map[string]any{"ok": false, "message": "the cache has 64 uploads in progress, the most it takes"}; !reflect.DeepEqual(out, want) {
		t.Errorf("a 65th upload = %v; want %v", out, want)
	}
}

// TestFinishedEntriesDoNotChange: an upload that is still being written cannot
// be finalized, so no write lands in an entry.
func TestFinishedEntriesDoNotChange(t *testing.T) {
	b := newBox(t, 1<<20)
	u := b.reserve(mainToken, "k")
	capability := strings.TrimPrefix(u, b.srv.URL+"/cache/upload/")
	r, w := io.Pipe()
	done := make(chan error)
	go func() { done <- b.store.Put(capability, "", r, b.clock()) }()
	w.Write([]byte("good"))
	_, out := b.call(mainToken, "FinalizeCacheEntryUpload", map[string]string{"key": "k", "version": "v1", "size_bytes": "4"})
	if want := map[string]any{"ok": false, "message": "the upload is still being written"}; !reflect.DeepEqual(out, want) {
		t.Errorf("finalize during a write = %v; want %v", out, want)
	}
	w.Write([]byte("more"))
	w.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, out := b.call(mainToken, "FinalizeCacheEntryUpload", map[string]string{"key": "k", "version": "v1", "size_bytes": "8"}); out["ok"] != true {
		t.Fatalf("finalize after the write = %v", out)
	}
	if _, archive := b.restore(mainToken, "k", "v1"); archive != "goodmore" {
		t.Errorf("restored %q; want goodmore", archive)
	}
}

func TestLongBlockIDs(t *testing.T) {
	b := newBox(t, 1<<20)
	u := b.reserve(mainToken, "k")
	id := strings.Repeat("A", 128)
	if got := b.put(u+"?comp=block&blockid="+id, "long id"); got != http.StatusCreated {
		t.Fatalf("a block with a 128-character id = %d", got)
	}
	b.put(u+"?comp=blocklist", "<BlockList><Latest>"+id+"</Latest></BlockList>")
	b.call(mainToken, "FinalizeCacheEntryUpload", map[string]string{"key": "k", "version": "v1", "size_bytes": "7"})
	if _, archive := b.restore(mainToken, "k", "v1"); archive != "long id" {
		t.Errorf("restored %q; want the block", archive)
	}
}

// TestRepositoriesShareTheCache: a full cache gives up the entries of the
// repository that holds the most, so one cannot push out the others.
func TestRepositoriesShareTheCache(t *testing.T) {
	b := newBox(t, 20)
	b.save(mainToken, "a", "v1", "aaaaaaaa")
	b.advance(time.Minute)
	b.save(mainToken, "b", "v1", "bbbbbbbb")
	b.advance(time.Minute)
	b.save(otherToken, "c", "v1", "cccccccc")
	b.advance(time.Minute)
	b.save(otherToken, "d", "v1", "dddddddd")
	var kept []string
	for _, c := range []struct{ token, key string }{{mainToken, "a"}, {mainToken, "b"}, {otherToken, "c"}, {otherToken, "d"}} {
		if got, _ := b.restore(c.token, c.key, "v1"); got != "" {
			kept = append(kept, got)
		}
	}
	if want := []string{"b", "d"}; !reflect.DeepEqual(kept, want) {
		t.Errorf("kept %v; want %v, one entry of each repository", kept, want)
	}
}

func TestEntriesPerRepository(t *testing.T) {
	b := newBox(t, 1<<30)
	c := Claims{Repo: "100", Scopes: []Scope{{"refs/heads/main", 3}}}
	for i := range 10001 {
		capability, err := b.store.Reserve(c, fmt.Sprint("k", i), "v1", testTime.Add(time.Duration(i)*time.Second))
		if err == nil {
			err = b.store.Put(capability, "", strings.NewReader("x"), testTime)
		}
		if err == nil {
			_, err = b.store.Finalize(c, fmt.Sprint("k", i), "v1", 1, testTime.Add(time.Duration(i)*time.Second))
		}
		if err != nil {
			t.Fatalf("entry %d: %v", i, err)
		}
	}
	if size, n := b.store.Size(); size != 10000 || n != 10000 {
		t.Errorf("Size = %d in %d; want 10000 in 10000", size, n)
	}
	if _, _, ok := b.store.Match(c, "k0", nil, "v1", testTime); ok {
		t.Errorf("the oldest entry stayed")
	}
}

// TestJoiningCountsTowardTheCache: while the blocks are joined, the block
// being copied takes room twice, and that room counts.
func TestJoiningCountsTowardTheCache(t *testing.T) {
	b := newBox(t, 1000)
	fits, tight := b.reserve(mainToken, "fits"), b.reserve(mainToken, "tight")
	b.put(fits+"?comp=block&blockid=b0", strings.Repeat("x", 200))
	b.put(fits+"?comp=block&blockid=b1", strings.Repeat("x", 200))
	b.put(tight+"?comp=block&blockid=b0", strings.Repeat("x", 300))
	b.put(tight+"?comp=block&blockid=b1", strings.Repeat("x", 300))
	list := "?comp=blocklist"
	if got := b.put(tight+list, "<BlockList><Latest>b0</Latest><Latest>b1</Latest></BlockList>"); got != http.StatusBadRequest {
		t.Errorf("joining 300 bytes beside 1000 taken = %d; want 400", got)
	}
	if got := b.put(fits+list, "<BlockList><Latest>b0</Latest><Latest>b1</Latest></BlockList>"); got != http.StatusCreated {
		t.Errorf("joining 200 bytes beside 400 taken = %d; want 201", got)
	}
	if b.onDisk() != 400 {
		t.Errorf("uploads take %d bytes; want 400, the joined archive", b.onDisk())
	}
}

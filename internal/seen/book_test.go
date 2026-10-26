package seen

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func (c *clock) tick(d time.Duration) { c.t = c.t.Add(d) }

func newBook() (*Book, *clock) {
	c := &clock{time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)}
	return &Book{Now: c.now}, c
}

var (
	api = netip.MustParseAddr("203.0.113.9")
	cdn = netip.MustParseAddr("198.51.100.7")
)

func TestNoteCountsRepeats(t *testing.T) {
	b, c := newBook()
	if _, isNew := b.Note("octo/site", api, 443, 6, ""); !isNew {
		t.Error("the first connection was not new")
	}
	c.tick(time.Minute)
	if d, isNew := b.Note("octo/site", api, 443, 6, "api.example.com"); isNew || d.Count != 2 || d.Name != "api.example.com" {
		t.Errorf("the second connection to the same place = %+v, new %v; want a count of 2 and not new", d, isNew)
	}
	c.tick(time.Minute)
	if _, isNew := b.Note("octo/site", api, 443, 17, ""); !isNew {
		t.Error("udp to the same port was not new")
	}
	if _, isNew := b.Note("octo/other", api, 443, 6, ""); !isNew {
		t.Error("another repository's first connection was not new")
	}

	got := b.Repos(0)
	if len(got) != 2 || got[0].Repo != "octo/other" || got[1].Repo != "octo/site" {
		t.Fatalf("Repos = %+v, want octo/other and octo/site", got)
	}
	want := []Destination{
		{Addr: "203.0.113.9", Port: 443, Proto: "udp", First: c.t, Last: c.t, Count: 1},
		{Addr: "203.0.113.9", Port: 443, Proto: "tcp", Name: "api.example.com", First: c.t.Add(-2 * time.Minute), Last: c.t.Add(-time.Minute), Count: 2},
	}
	for i, w := range want {
		if g := got[1].Destinations[i]; g != w {
			t.Errorf("destination %d = %+v, want %+v", i, g, w)
		}
	}
}

func TestNoteIgnoresPortsOfProtocolsWithout(t *testing.T) {
	b, _ := newBook()
	b.Note("octo/site", api, 1234, 1, "")
	b.Note("octo/site", netip.MustParseAddr("::ffff:203.0.113.9"), 0, 1, "")
	got := b.Repos(0)[0].Destinations
	if len(got) != 1 || got[0].Addr != "203.0.113.9" || got[0].Port != 0 || got[0].Proto != "icmp" || got[0].Count != 2 {
		t.Errorf("destinations = %+v, want one icmp destination at 203.0.113.9 seen twice", got)
	}
}

func TestStrings(t *testing.T) {
	for _, c := range []struct {
		d    Destination
		want string
	}{
		{Destination{Addr: "203.0.113.9", Port: 443, Proto: "tcp"}, "203.0.113.9:443"},
		{Destination{Addr: "203.0.113.9", Port: 443, Proto: "tcp", Name: "api.example.com"}, "203.0.113.9:443 (api.example.com)"},
		{Destination{Addr: "10.0.0.53", Port: 53, Proto: "udp"}, "10.0.0.53:53 udp"},
		{Destination{Addr: "2001:db8::1", Port: 443, Proto: "tcp"}, "[2001:db8::1]:443"},
		{Destination{Addr: "203.0.113.9", Proto: "icmp"}, "203.0.113.9 icmp"},
	} {
		if got := c.d.String(); got != c.want {
			t.Errorf("%+v = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestReposCapsToTheNewest(t *testing.T) {
	b, c := newBook()
	for i := range 5 {
		b.Note("octo/site", netip.MustParseAddr(fmt.Sprintf("203.0.113.%d", i+1)), 443, 6, "")
		c.tick(time.Second)
	}
	got := b.Repos(2)[0].Destinations
	if len(got) != 2 || got[0].Addr != "203.0.113.5" || got[1].Addr != "203.0.113.4" {
		t.Errorf("destinations = %+v, want 203.0.113.5 then 203.0.113.4", got)
	}
}

func TestRepositoryKeepsMaxPerRepoByLeastRecentUse(t *testing.T) {
	b, c := newBook()
	for i := range MaxPerRepo {
		b.Note("octo/site", netip.MustParseAddr(fmt.Sprintf("10.1.%d.%d", i/200, i%200+1)), 443, 6, "")
		c.tick(time.Second)
	}
	// Using the oldest one again makes the second oldest the least recently used.
	b.Note("octo/site", netip.MustParseAddr("10.1.0.1"), 443, 6, "")
	c.tick(time.Second)
	if _, isNew := b.Note("octo/site", api, 443, 6, ""); !isNew {
		t.Fatal("a new destination beyond the cap was not new")
	}
	got := b.Repos(0)[0].Destinations
	if len(got) != MaxPerRepo {
		t.Fatalf("%d destinations, want %d", len(got), MaxPerRepo)
	}
	for _, d := range got {
		if d.Addr == "10.1.0.2" {
			t.Error("the least recently used destination 10.1.0.2 is still there")
		}
	}
	if got[0].Addr != "203.0.113.9" {
		t.Errorf("newest = %s, want 203.0.113.9", got[0].Addr)
	}
}

func TestBookKeepsMaxRepos(t *testing.T) {
	b, c := newBook()
	for i := range MaxRepos {
		b.Note(fmt.Sprintf("octo/r%02d", i), api, 443, 6, "")
		c.tick(time.Second)
	}
	b.Note("octo/r00", api, 443, 6, "") // r00 connects again, so r01 is the quietest
	c.tick(time.Second)
	b.Note("octo/new", api, 443, 6, "")
	repos := b.Repos(0)
	if len(repos) != MaxRepos {
		t.Fatalf("%d repositories, want %d", len(repos), MaxRepos)
	}
	for _, r := range repos {
		if r.Repo == "octo/r01" {
			t.Error("the quietest repository octo/r01 is still there")
		}
	}
	if repos[0].Repo != "octo/new" {
		t.Errorf("first repository = %s, want octo/new", repos[0].Repo)
	}
}

func TestSaveAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "destinations.json")
	b, c := newBook()
	b.Note("octo/site", api, 443, 6, "api.example.com")
	c.tick(time.Hour)
	b.Note("octo/site", cdn, 80, 6, "")
	if err := b.Save(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Error("the temporary file is still there")
	}

	again := &Book{Now: c.now}
	if err := again.Load(path); err != nil {
		t.Fatal(err)
	}
	got, want := again.Repos(0), b.Repos(0)
	if len(got) != 1 || len(got[0].Destinations) != 2 || got[0].Destinations[0] != want[0].Destinations[0] || got[0].Destinations[1] != want[0].Destinations[1] {
		t.Errorf("loaded %+v, want %+v", got, want)
	}

	// Nothing changed since, so nothing is written.
	os.Remove(path)
	if err := b.Save(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("Save wrote a book that did not change")
	}
}

func TestLoadForgetsWhatNobodyUsed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "destinations.json")
	b, c := newBook()
	b.Note("octo/old", api, 443, 6, "")
	c.tick(Keep - time.Hour)
	b.Note("octo/site", cdn, 443, 6, "")
	if err := b.Save(path); err != nil {
		t.Fatal(err)
	}
	c.tick(2 * time.Hour)
	again := &Book{Now: c.now}
	if err := again.Load(path); err != nil {
		t.Fatal(err)
	}
	if repos := again.Repos(0); len(repos) != 1 || repos[0].Repo != "octo/site" {
		t.Errorf("loaded %+v, want only octo/site", repos)
	}
}

func TestLoadMissingAndBroken(t *testing.T) {
	b, _ := newBook()
	if err := b.Load(filepath.Join(t.TempDir(), "none.json")); err != nil {
		t.Errorf("a missing file is an error: %v", err)
	}
	path := filepath.Join(t.TempDir(), "destinations.json")
	os.WriteFile(path, []byte("{"), 0o644)
	if err := b.Load(path); err == nil {
		t.Error("a broken file loaded")
	}
}

package egress

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

// record does what the agent does with a refusal.
func record(l *Ledger, class string, b Block) bool {
	if !l.Admit(class, b.Kind, b.Dest) {
		return false
	}
	l.Keep(class, b)
	return true
}

func TestLedgerRecordsARefusalOncePerMinute(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := &Ledger{Now: func() time.Time { return now }}
	b := Block{Kind: Packet, Dest: "203.0.113.9:443", Runner: "box-1-agent-ab12cd"}
	if !record(l, "agent", b) {
		t.Fatal("the first refusal was not recorded")
	}
	now = now.Add(59 * time.Second)
	if record(l, "agent", b) {
		t.Error("the same refusal 59 seconds later was recorded")
	}
	if !record(l, "agent", Block{Kind: Lookup, Dest: "203.0.113.9:443"}) || !record(l, "other", b) {
		t.Error("a lookup of the same text, or another class's refusal, was taken for a repeat")
	}
	now = now.Add(2 * time.Second)
	if !record(l, "agent", b) {
		t.Error("the same refusal a minute later was not recorded")
	}
	got := l.Counts("agent").Recent
	if len(got) != 3 || got[0].Time != time.Unix(1_000_000, 0) || got[0].Runner != "box-1-agent-ab12cd" || got[2].Time != now {
		t.Errorf("Recent = %+v", got)
	}
}

func TestLedgerLimitsWhatItRecordsPerMinuteAndKeepsTheLatest(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := &Ledger{Now: func() time.Time { return now }}
	recorded := 0
	for i := range 100 {
		if record(l, "agent", Block{Kind: Packet, Dest: fmt.Sprintf("203.0.113.%d:443", i)}) {
			recorded++
		}
	}
	if recorded != perMinute {
		t.Errorf("recorded %d refusals in a minute, want %d", recorded, perMinute)
	}
	got := l.Counts("agent").Recent
	if len(got) != recent || got[len(got)-1].Dest != "203.0.113.29:443" || got[0].Dest != "203.0.113.10:443" {
		t.Errorf("Recent = %v, want the last %d of the %d recorded", got, recent, perMinute)
	}
	now = now.Add(time.Minute)
	if !record(l, "agent", Block{Kind: Packet, Dest: "203.0.113.200:443"}) {
		t.Error("a new minute did not reset the limit")
	}
}

func TestLedgerCounts(t *testing.T) {
	l := &Ledger{}
	l.CountLookup("agent")
	l.CountLookup("agent")
	l.SetPackets("agent", 41)
	l.SetPackets("agent", 42)
	if got, want := l.Counts("agent"), (Counts{Packets: 42, Lookups: 2}); !reflect.DeepEqual(got, want) {
		t.Errorf("Counts = %+v, want %+v", got, want)
	}
	if got := l.Counts("unknown"); got.Packets != 0 || got.Lookups != 0 || got.Recent != nil {
		t.Errorf("Counts(unknown) = %+v", got)
	}
}

func TestLedgerGivesEachKindItsOwnShareOfAMinute(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := &Ledger{Now: func() time.Time { return now }}
	for i := range perMinute {
		l.Admit("agent", Packet, fmt.Sprintf("203.0.113.%d:443", i))
	}
	if l.Admit("agent", Packet, "203.0.113.200:443") {
		t.Error("a packet past the share was admitted")
	}
	if !l.Admit("agent", Kind("withheld"), "api.example.com 10.0.0.1") || !l.Admit("agent", Lookup, "data.evil.example") {
		t.Error("another kind was refused because packets used the share")
	}
}

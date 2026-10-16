package dnsfwd

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/egress"
)

// fakeUpstream answers each name with the records it was given.
type fakeUpstream struct {
	mu      sync.Mutex
	answers map[string][]rr
	err     error
	asked   []string
	tcp     []bool
}

func (f *fakeUpstream) Exchange(_ context.Context, query []byte, tcp bool) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q, _ := ParseQuery(query)
	f.asked = append(f.asked, q.Name)
	f.tcp = append(f.tcp, tcp)
	if f.err != nil {
		return nil, f.err
	}
	return response(query, 0, f.answers[q.Name]...), nil
}

var classNet = netip.MustParsePrefix("172.30.0.0/24")

// recorder keeps what a server opened and reported.
type recorder struct {
	mu     sync.Mutex
	grants []Grant
	events []Event
}

func (r *recorder) open(_ context.Context, g []Grant) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.grants = append(r.grants, g...)
	return nil
}

func (r *recorder) event(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) opened() []Grant {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.grants)
}

func (r *recorder) reported() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

func newServer(t *testing.T, entries []string, up *fakeUpstream) (*Server, *recorder) {
	t.Helper()
	policy, err := egress.Parse(append([]string{"github"}, entries...))
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	return &Server{Policy: policy, Clients: classNet, Upstream: up, Open: rec.open, Event: rec.event}, rec
}

var job = netip.MustParseAddr("172.30.0.7")

func TestHandleOpensTheAddressesOfAnAllowedNameBeforeAnswering(t *testing.T) {
	up := &fakeUpstream{answers: map[string][]rr{"api.example.com": {cname(60, "edge.example.net"), a(120, "203.0.113.5"), a(120, "2001:db8::5")}}}
	s, rec := newServer(t, []string{"api.example.com:8443"}, up)
	var order []string
	s.Open = func(ctx context.Context, g []Grant) error {
		order = append(order, "open")
		return rec.open(ctx, g)
	}
	q := query(11, "API.example.com", TypeA)

	got := s.Handle(context.Background(), q, job, false)

	want := response(q, 0, up.answers["api.example.com"]...)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Handle = % x, want the upstream's answer % x", got, want)
	}
	wantGrants := []Grant{
		{Addr: netip.MustParseAddr("203.0.113.5"), Ports: egress.Ports{List: []uint16{8443}}, TTL: MinOpen},
		{Addr: netip.MustParseAddr("2001:db8::5"), Ports: egress.Ports{List: []uint16{8443}}, TTL: MinOpen},
	}
	if !reflect.DeepEqual(rec.opened(), wantGrants) {
		t.Errorf("opened %+v, want %+v", rec.opened(), wantGrants)
	}
	if len(rec.reported()) != 0 || !reflect.DeepEqual(order, []string{"open"}) {
		t.Errorf("events %+v, order %v", rec.reported(), order)
	}
	if name, ok := s.NameOf(netip.MustParseAddr("203.0.113.5")); !ok || name != "api.example.com" {
		t.Errorf("NameOf = %q, %v", name, ok)
	}
}

func TestHandleRefusesANameThePolicyDoesNotAllowWithoutAskingUpstream(t *testing.T) {
	up := &fakeUpstream{}
	s, rec := newServer(t, nil, up)
	q := query(5, "data.evil.example", TypeA)

	got := s.Handle(context.Background(), q, job, false)

	if want := Reply(q, RcodeRefused); !reflect.DeepEqual(got, want) {
		t.Errorf("Handle = % x, want REFUSED % x", got, want)
	}
	if len(up.asked) != 0 || len(rec.opened()) != 0 {
		t.Errorf("asked upstream %v and opened %v", up.asked, rec.opened())
	}
	if want := []Event{{Kind: Refused, From: job, Name: "data.evil.example", Type: TypeA}}; !reflect.DeepEqual(rec.reported(), want) {
		t.Errorf("events = %+v, want %+v", rec.reported(), want)
	}
}

func TestHandleDropsQueriesFromOutsideTheClassNetwork(t *testing.T) {
	up := &fakeUpstream{answers: map[string][]rr{"github.com": {a(60, "140.82.112.3")}}}
	s, rec := newServer(t, nil, up)
	for _, from := range []string{"172.31.0.7", "127.0.0.1", "10.0.0.5"} {
		if got := s.Handle(context.Background(), query(1, "github.com", TypeA), netip.MustParseAddr(from), false); got != nil {
			t.Errorf("Handle from %s = % x, want none", from, got)
		}
	}
	if len(up.asked) != 0 || len(rec.reported()) != 0 {
		t.Errorf("asked %v, events %+v", up.asked, rec.reported())
	}
}

func TestHandleKeepsAddressesOpenFromTheTTLWithinBounds(t *testing.T) {
	up := &fakeUpstream{answers: map[string][]rr{"github.com": {
		a(0, "140.82.112.1"), a(600, "140.82.112.2"), a(86400, "140.82.112.3"),
	}}}
	s, rec := newServer(t, nil, up)
	s.Handle(context.Background(), query(1, "github.com", TypeA), job, false)
	var ttls []time.Duration
	for _, g := range rec.opened() {
		ttls = append(ttls, g.TTL)
	}
	if want := []time.Duration{5 * time.Minute, 10 * time.Minute, time.Hour}; !reflect.DeepEqual(ttls, want) {
		t.Errorf("TTLs = %v, want %v", ttls, want)
	}
}

func TestHandleKeepsPrivateAddressesForHostsNamedInFull(t *testing.T) {
	up := &fakeUpstream{answers: map[string][]rr{
		"db.internal.example":    {a(60, "10.20.1.5")},
		"app.wild.example":       {a(60, "10.20.1.6"), a(60, "203.0.113.9")},
		"meta.internal.example":  {a(60, "169.254.169.254"), a(60, "127.0.0.1")},
		"other.internal.example": {a(60, "100.64.1.1")},
	}}
	s, rec := newServer(t, []string{"db.internal.example", "*.wild.example", "meta.internal.example", "*.internal.example"}, up)
	for _, name := range []string{"db.internal.example", "app.wild.example", "meta.internal.example", "other.internal.example"} {
		s.Handle(context.Background(), query(1, name, TypeA), job, false)
	}
	var got []string
	for _, g := range rec.opened() {
		got = append(got, g.Addr.String())
	}
	if want := []string{"10.20.1.5", "203.0.113.9"}; !reflect.DeepEqual(got, want) {
		t.Errorf("opened %v, want %v", got, want)
	}
	var withheld []string
	for _, e := range rec.reported() {
		if e.Kind != Withheld {
			t.Errorf("event %+v is not a withheld address", e)
		}
		withheld = append(withheld, e.Addr.String())
	}
	if want := []string{"10.20.1.6", "169.254.169.254", "127.0.0.1", "100.64.1.1"}; !reflect.DeepEqual(withheld, want) {
		t.Errorf("withheld %v, want %v", withheld, want)
	}
}

func TestHandleFailsClosedWhenTheAddressesCannotBeOpened(t *testing.T) {
	up := &fakeUpstream{answers: map[string][]rr{"github.com": {a(60, "140.82.112.3")}}}
	s, rec := newServer(t, nil, up)
	s.Open = func(context.Context, []Grant) error { return errors.New("nft: exit status 1") }
	q := query(3, "github.com", TypeA)
	if got := s.Handle(context.Background(), q, job, false); !reflect.DeepEqual(got, Reply(q, RcodeServFail)) {
		t.Errorf("Handle = % x, want SERVFAIL", got)
	}
	if len(rec.reported()) != 1 || (rec.reported())[0].Kind != Failed {
		t.Errorf("events = %+v", rec.reported())
	}

	up.err = errors.New("timeout")
	s.Open = nil
	if got := s.Handle(context.Background(), q, job, false); !reflect.DeepEqual(got, Reply(q, RcodeServFail)) {
		t.Errorf("Handle with upstream down = % x, want SERVFAIL", got)
	}
}

func TestHandleSpendsABudgetOfLookups(t *testing.T) {
	up := &fakeUpstream{answers: map[string][]rr{"github.com": {a(60, "140.82.112.3")}}}
	s, _ := newServer(t, nil, up)
	now := time.Unix(1_000_000, 0)
	s.Now = func() time.Time { return now }
	q := query(1, "github.com", TypeA)
	for i := range burst {
		if got := s.Handle(context.Background(), q, job, false); !reflect.DeepEqual(got, response(q, 0, up.answers["github.com"]...)) {
			t.Fatalf("lookup %d was not answered", i)
		}
	}
	if got := s.Handle(context.Background(), q, job, false); !reflect.DeepEqual(got, Reply(q, RcodeServFail)) {
		t.Errorf("lookup %d = % x, want SERVFAIL", burst+1, got)
	}
	now = now.Add(time.Second)
	if got := s.Handle(context.Background(), q, job, false); reflect.DeepEqual(got, Reply(q, RcodeServFail)) {
		t.Error("a second later the budget did not refill")
	}
}

func TestHandleAnswersAMalformedQueryWithFormErr(t *testing.T) {
	s, _ := newServer(t, nil, &fakeUpstream{})
	q := query(2, "github.com", TypeA)
	q[5] = 2
	if got := s.Handle(context.Background(), q, job, false); len(got) < 4 || got[3]&0xf != RcodeFormErr {
		t.Errorf("Handle = % x, want FORMERR", got)
	}
}

func TestServeUDPAnswersFromTheClassNetwork(t *testing.T) {
	up := &fakeUpstream{answers: map[string][]rr{"github.com": {a(60, "140.82.112.3")}}}
	s, rec := newServer(t, nil, up)
	s.Clients = netip.MustParsePrefix("127.0.0.0/8")
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- s.ServeUDP(ctx, pc) }()

	conn, err := net.Dial("udp", pc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for name, wantRcode := range map[string]byte{"github.com": 0, "evil.example": RcodeRefused} {
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		q := query(0x4242, name, TypeA)
		if _, err := conn.Write(q); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 512)
		n, err := conn.Read(buf)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if binary.BigEndian.Uint16(buf) != 0x4242 || buf[3]&0xf != wantRcode {
			t.Errorf("%s: answer % x, want code %d", name, buf[:n], wantRcode)
		}
	}
	if len(rec.opened()) != 1 {
		t.Errorf("opened %+v", rec.opened())
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("ServeUDP = %v", err)
	}
}

func TestServeTCPAnswersSeveralQueriesOnOneConnection(t *testing.T) {
	up := &fakeUpstream{answers: map[string][]rr{"github.com": {a(60, "140.82.112.3")}}}
	s, _ := newServer(t, nil, up)
	s.Clients = netip.MustParsePrefix("127.0.0.0/8")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- s.ServeTCP(ctx, l) }()

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	for i, name := range []string{"github.com", "evil.example", "api.github.com"} {
		q := query(uint16(i), name, TypeA)
		if _, err := conn.Write(append([]byte{byte(len(q) >> 8), byte(len(q))}, q...)); err != nil {
			t.Fatal(err)
		}
		var length [2]byte
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			t.Fatal(err)
		}
		resp := make([]byte, int(length[0])<<8|int(length[1]))
		if _, err := io.ReadFull(conn, resp); err != nil {
			t.Fatal(err)
		}
		if got, want := resp[3]&0xf, map[string]byte{"github.com": 0, "evil.example": RcodeRefused, "api.github.com": 0}[name]; got != want {
			t.Errorf("%s: code %d, want %d", name, got, want)
		}
	}
	if !reflect.DeepEqual(up.tcp, []bool{true, true}) {
		t.Errorf("upstream was asked over TCP: %v, want both allowed names", up.tcp)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("ServeTCP = %v", err)
	}
}

func TestResolverTriesTheNextServerWhenOneDoesNotAnswer(t *testing.T) {
	dead, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer dead.Close()
	live, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := live.ReadFrom(buf)
			if err != nil {
				return
			}
			live.WriteTo(response(buf[:n], 0, a(30, "203.0.113.1")), addr)
		}
	}()
	r := &Resolver{
		Timeout: 200 * time.Millisecond,
		Servers: func() []netip.AddrPort {
			return []netip.AddrPort{dead.LocalAddr().(*net.UDPAddr).AddrPort(), live.LocalAddr().(*net.UDPAddr).AddrPort()}
		},
	}
	q := query(0x0102, "github.com", TypeA)
	got, err := r.Exchange(context.Background(), q, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, response(q, 0, a(30, "203.0.113.1"))) {
		t.Errorf("Exchange = % x", got)
	}
	if _, err := (&Resolver{Servers: func() []netip.AddrPort { return nil }}).Exchange(context.Background(), q, false); err == nil {
		t.Error("Exchange with no resolver succeeded")
	}
}

func TestParseResolvConf(t *testing.T) {
	got := ParseResolvConf("# comment\nsearch lan\nnameserver 127.0.0.53\nnameserver fe80::1%eth0\nnameserver nonsense\noptions edns0\n")
	want := []netip.AddrPort{netip.MustParseAddrPort("127.0.0.53:53"), netip.MustParseAddrPort("[fe80::1%eth0]:53")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseResolvConf = %v, want %v", got, want)
	}
}

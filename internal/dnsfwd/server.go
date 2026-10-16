package dnsfwd

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dominikzabcik/boxrunner/internal/egress"
)

// How long an address stays open after an answer names it: the answer's TTL,
// kept within these bounds. A client may keep using an answer a while after
// its TTL, and an answer with TTL 0 should still be usable.
const (
	MinOpen = 5 * time.Minute
	MaxOpen = time.Hour
)

const (
	maxInFlight = 64
	// A class's lookups, in all: a job that resolves hundreds of names at
	// once, as a package manager does, still fits.
	rate  = 500
	burst = 1000
	// nameBookSize is how many addresses the server remembers a name for.
	nameBookSize = 4096
)

// Upstream is the resolver that answers the names the policy allows.
type Upstream interface {
	// Exchange sends a query and returns the response's bytes.
	Exchange(ctx context.Context, query []byte, tcp bool) ([]byte, error)
}

// Kind says what an Event reports.
type Kind int

const (
	// Refused: the policy does not allow the name.
	Refused Kind = iota
	// Withheld: an allowed name resolved to an address the policy keeps
	// closed, so the address was not opened.
	Withheld
	// Failed: the forwarder could not answer an allowed name.
	Failed
)

// Event is something the forwarder did that the box's owner may want to see.
type Event struct {
	Kind Kind
	From netip.Addr
	Name string
	Type uint16
	// Addr and Reason belong to Withheld, Err to Failed.
	Addr   netip.Addr
	Reason string
	Err    error
}

// Server answers the lookups of one class's jobs.
type Server struct {
	Policy egress.Policy
	// Clients are the addresses lookups may come from: the class's network.
	Clients  netip.Prefix
	Upstream Upstream
	// Open makes the grants reachable. The answer goes back only when it
	// succeeds, so a name never resolves to an address the job cannot use.
	Open  func(ctx context.Context, grants []egress.Grant) error
	Event func(Event)
	Now   func() time.Time

	mu     sync.Mutex
	names  map[netip.Addr]seen
	tokens float64
	last   time.Time
}

type seen struct {
	name    string
	expires time.Time
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) event(e Event) {
	if s.Event != nil {
		s.Event(e)
	}
}

// NameOf is the allowed name an address was last opened for, while it is open.
func (s *Server) NameOf(addr netip.Addr) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.names[addr]
	return n.name, ok && s.now().Before(n.expires)
}

func (s *Server) remember(addr netip.Addr, name string, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.names == nil {
		s.names = map[netip.Addr]seen{}
	}
	if len(s.names) >= nameBookSize {
		now := s.now()
		for a, n := range s.names {
			if !now.Before(n.expires) {
				delete(s.names, a)
			}
		}
		if len(s.names) >= nameBookSize {
			clear(s.names)
		}
	}
	s.names[addr] = seen{name, s.now().Add(ttl)}
}

// allow takes one lookup from the class's budget.
func (s *Server) allow() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if s.last.IsZero() {
		s.tokens, s.last = burst, now
	}
	s.tokens = min(burst, s.tokens+now.Sub(s.last).Seconds()*rate)
	s.last = now
	if s.tokens < 1 {
		return false
	}
	s.tokens--
	return true
}

// Handle answers one query. It returns nil when the query is not worth an
// answer: it came from outside the class's network, or is cut off.
func (s *Server) Handle(ctx context.Context, query []byte, from netip.Addr, tcp bool) []byte {
	if !s.Clients.Contains(from.Unmap()) {
		return nil
	}
	q, err := ParseQuery(query)
	if err != nil {
		return Reply(query, RcodeFormErr)
	}
	ports, _, ok := s.Policy.Match(q.Name)
	if !ok {
		s.event(Event{Kind: Refused, From: from, Name: q.Name, Type: q.Type})
		return Reply(query, RcodeRefused)
	}
	if q.Type == TypeAAAA {
		// A job's network has no IPv6. An empty answer tells its resolver so at
		// once, where an address would make it try, wait and fall back.
		return Reply(query, RcodeSuccess)
	}
	if !s.allow() {
		return Reply(query, RcodeServFail)
	}
	response, err := s.Upstream.Exchange(ctx, query, tcp)
	if err != nil {
		s.event(Event{Kind: Failed, From: from, Name: q.Name, Type: q.Type, Err: err})
		return Reply(query, RcodeServFail)
	}
	answer, err := ParseAnswer(response)
	if err != nil {
		s.event(Event{Kind: Failed, From: from, Name: q.Name, Type: q.Type, Err: err})
		return Reply(query, RcodeServFail)
	}
	if answer.Question != q {
		s.event(Event{Kind: Failed, From: from, Name: q.Name, Type: q.Type, Err: errors.New("the answer is to another question")})
		return Reply(query, RcodeServFail)
	}
	var grants []egress.Grant
	for _, r := range answer.Reachable() {
		addr := r.Addr.Unmap()
		if !addr.Is4() {
			continue
		}
		_, ownerExact, _ := s.Policy.Match(r.Name)
		reason := ""
		switch {
		case egress.IsNever(addr):
			reason = "an address no entry can allow"
		case egress.IsPrivate(addr) && !ownerExact:
			reason = "a private address, which only a host named in full may have"
		}
		if reason != "" {
			s.event(Event{Kind: Withheld, From: from, Name: q.Name, Type: q.Type, Addr: addr, Reason: reason})
			continue
		}
		ttl := min(max(time.Duration(r.TTL)*time.Second, MinOpen), MaxOpen)
		grants = append(grants, egress.Grant{Addr: addr, Ports: ports, TTL: ttl})
	}
	if len(grants) > 0 && s.Open != nil {
		if err := s.Open(ctx, grants); err != nil {
			s.event(Event{Kind: Failed, From: from, Name: q.Name, Type: q.Type, Err: err})
			return Reply(query, RcodeServFail)
		}
	}
	for _, g := range grants {
		s.remember(g.Addr, q.Name, g.TTL)
	}
	return response
}

// ServeUDP answers datagrams on pc until ctx ends.
func (s *Server) ServeUDP(ctx context.Context, pc net.PacketConn) error {
	stop := context.AfterFunc(ctx, func() { pc.Close() })
	defer stop()
	slots := make(chan struct{}, maxInFlight)
	buf := make([]byte, 65535)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		udp, ok := addr.(*net.UDPAddr)
		if !ok {
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			continue
		}
		query := slices.Clone(buf[:n])
		go func() {
			defer func() { <-slots }()
			qctx, cancel := context.WithTimeout(ctx, queryTimeout)
			defer cancel()
			if response := s.Handle(qctx, query, udp.AddrPort().Addr(), false); response != nil {
				pc.WriteTo(response, addr)
			}
		}()
	}
}

// ServeTCP answers the queries of connections on l until ctx ends.
func (s *Server) ServeTCP(ctx context.Context, l net.Listener) error {
	stop := context.AfterFunc(ctx, func() { l.Close() })
	defer stop()
	slots := make(chan struct{}, maxInFlight)
	var mu sync.Mutex
	open := map[netip.Addr]int{}
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() == nil {
				return err
			}
			return nil
		}
		from := netip.Addr{}
		if tcp, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
			from = tcp.AddrPort().Addr()
		}
		// A client that opens connections and sends nothing must not take every slot.
		mu.Lock()
		over := open[from] >= maxPerSource
		if !over {
			open[from]++
		}
		mu.Unlock()
		if over {
			conn.Close()
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			conn.Close()
			mu.Lock()
			open[from]--
			mu.Unlock()
			continue
		}
		go func() {
			defer func() {
				<-slots
				mu.Lock()
				if open[from]--; open[from] == 0 {
					delete(open, from)
				}
				mu.Unlock()
			}()
			defer conn.Close()
			s.serveConn(ctx, conn)
		}()
	}
}

const (
	queryTimeout = 10 * time.Second
	idleTimeout  = 10 * time.Second
	// maxQueries is how many queries one connection may send, and maxPerSource
	// how many connections one address may hold.
	maxQueries   = 64
	maxPerSource = 8
)

func (s *Server) serveConn(ctx context.Context, conn net.Conn) {
	tcp, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return
	}
	for range maxQueries {
		conn.SetReadDeadline(time.Now().Add(idleTimeout))
		var length [2]byte
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			return
		}
		query := make([]byte, int(length[0])<<8|int(length[1]))
		if _, err := io.ReadFull(conn, query); err != nil {
			return
		}
		qctx, cancel := context.WithTimeout(ctx, queryTimeout)
		response := s.Handle(qctx, query, tcp.AddrPort().Addr(), true)
		cancel()
		if response == nil || len(response) > 0xffff {
			return
		}
		conn.SetWriteDeadline(time.Now().Add(idleTimeout))
		if _, err := conn.Write(append([]byte{byte(len(response) >> 8), byte(len(response))}, response...)); err != nil {
			return
		}
	}
}

// Resolver asks the box's own resolvers, the ones in /etc/resolv.conf.
type Resolver struct {
	// Servers lists the resolvers; nil reads /etc/resolv.conf.
	Servers func() []netip.AddrPort
	// Timeout is how long each resolver has to answer; 0 means five seconds.
	Timeout time.Duration
	// Dial replaces net.Dialer in tests.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)
}

// Exchange sends the query to each resolver in turn until one answers.
func (r *Resolver) Exchange(ctx context.Context, query []byte, tcp bool) ([]byte, error) {
	servers := r.Servers
	if servers == nil {
		servers = ResolvConf
	}
	list := servers()
	if len(list) == 0 {
		return nil, errors.New("dns: the box has no resolver in /etc/resolv.conf")
	}
	var errs []error
	for _, server := range list {
		response, err := r.exchange(ctx, server, query, tcp)
		if err == nil {
			return response, nil
		}
		errs = append(errs, err)
		if ctx.Err() != nil {
			break
		}
	}
	return nil, errors.Join(errs...)
}

func (r *Resolver) exchange(ctx context.Context, server netip.AddrPort, query []byte, tcp bool) ([]byte, error) {
	// The job picked the query's ID, and an attacker who knows it needs only
	// the resolver's address and our port to forge an answer. A fresh ID, and
	// an answer that must repeat the question, take that away.
	var id [2]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	sent := slices.Clone(query)
	sent[0], sent[1] = id[0], id[1]
	question, err := questionSection(sent)
	if err != nil {
		return nil, err
	}
	response, err := r.send(ctx, server, sent, id, tcp)
	if err != nil {
		return nil, err
	}
	if len(response) < headerLen || response[0] != id[0] || response[1] != id[1] || response[2]&0x80 == 0 || !bytes.Equal(responseQuestion(response), question) {
		return nil, errors.New("dns: the answer is not to our query")
	}
	response[0], response[1] = query[0], query[1]
	return response, nil
}

// questionSection is the bytes of a query's question.
func questionSection(msg []byte) ([]byte, error) {
	_, off, err := readName(msg, headerLen)
	if err != nil || off+4 > len(msg) {
		return nil, errors.New("dns: cut-off question")
	}
	return msg[headerLen : off+4], nil
}

// responseQuestion is the bytes of a response's first question, nil when it has none.
func responseQuestion(msg []byte) []byte {
	if binary.BigEndian.Uint16(msg[4:6]) < 1 {
		return nil
	}
	q, _ := questionSection(msg)
	return q
}

func (r *Resolver) send(ctx context.Context, server netip.AddrPort, query []byte, id [2]byte, tcp bool) ([]byte, error) {
	network := "udp"
	if tcp {
		network = "tcp"
	}
	dial := r.Dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	conn, err := dial(ctx, network, server.String())
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	conn.SetDeadline(time.Now().Add(timeout))
	if !tcp {
		if _, err := conn.Write(query); err != nil {
			return nil, err
		}
		buf := make([]byte, 65535)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return nil, err
			}
			// A datagram that is not the answer to this query is ignored.
			if n >= 2 && buf[0] == id[0] && buf[1] == id[1] {
				return slices.Clone(buf[:n]), nil
			}
		}
	}
	if _, err := conn.Write(append([]byte{byte(len(query) >> 8), byte(len(query))}, query...)); err != nil {
		return nil, err
	}
	var length [2]byte
	if _, err := io.ReadFull(conn, length[:]); err != nil {
		return nil, err
	}
	response := make([]byte, int(length[0])<<8|int(length[1]))
	if _, err := io.ReadFull(conn, response); err != nil {
		return nil, err
	}
	return response, nil
}

// ResolvConfPath is where ResolvConf reads the box's resolvers.
var ResolvConfPath = "/etc/resolv.conf"

// ResolvConf lists the resolvers of /etc/resolv.conf, at port 53.
func ResolvConf() []netip.AddrPort {
	b, err := os.ReadFile(ResolvConfPath)
	if err != nil {
		return nil
	}
	return ParseResolvConf(string(b))
}

// ParseResolvConf lists the nameserver lines of a resolv.conf.
func ParseResolvConf(content string) []netip.AddrPort {
	var servers []netip.AddrPort
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		addr, err := netip.ParseAddr(fields[1])
		if err != nil {
			continue
		}
		servers = append(servers, netip.AddrPortFrom(addr, 53))
	}
	return servers
}

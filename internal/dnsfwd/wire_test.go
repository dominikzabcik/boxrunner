package dnsfwd

import (
	"encoding/binary"
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

// name writes a name in wire format, without compression.
func name(n string) []byte {
	var b []byte
	for _, label := range strings.Split(n, ".") {
		b = append(append(b, byte(len(label))), label...)
	}
	return append(b, 0)
}

// query is a standard query with recursion desired.
func query(id uint16, n string, typ uint16) []byte {
	b := binary.BigEndian.AppendUint16(nil, id)
	b = append(b, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0)
	b = append(b, name(n)...)
	return binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16(b, typ), 1)
}

// rr is a resource record whose owner is a pointer to the question's name.
type rr struct {
	typ   uint16
	ttl   uint32
	rdata []byte
}

func a(ttl uint32, addr string) rr {
	ip := netip.MustParseAddr(addr)
	return rr{typ: map[bool]uint16{true: TypeA, false: TypeAAAA}[ip.Is4()], ttl: ttl, rdata: ip.AsSlice()}
}

func cname(ttl uint32, target string) rr { return rr{typ: 5, ttl: ttl, rdata: name(target)} }

// response answers q with records whose owners point at the question's name.
func response(q []byte, rcode int, records ...rr) []byte {
	b := append([]byte{}, q...)
	b[2], b[3] = 0x81, byte(0x80|rcode)
	binary.BigEndian.PutUint16(b[6:], uint16(len(records)))
	for _, r := range records {
		b = append(b, 0xc0, 0x0c)
		b = binary.BigEndian.AppendUint16(b, r.typ)
		b = binary.BigEndian.AppendUint16(b, 1)
		b = binary.BigEndian.AppendUint32(b, r.ttl)
		b = binary.BigEndian.AppendUint16(b, uint16(len(r.rdata)))
		b = append(b, r.rdata...)
	}
	return b
}

func TestParseQuery(t *testing.T) {
	got, err := ParseQuery(query(7, "API.Example.com", TypeAAAA))
	if err != nil {
		t.Fatal(err)
	}
	if want := (Question{Name: "api.example.com", Type: TypeAAAA, Class: 1}); got != want {
		t.Errorf("ParseQuery = %+v, want %+v", got, want)
	}
}

func TestParseQueryRefusesWhatIsNotOneStandardQuery(t *testing.T) {
	good := query(1, "example.com", TypeA)
	withFlags := func(f uint16) []byte {
		b := append([]byte{}, good...)
		binary.BigEndian.PutUint16(b[2:], f)
		return b
	}
	twoQuestions := append([]byte{}, good...)
	twoQuestions[5] = 2
	for label, msg := range map[string][]byte{
		"empty":            nil,
		"short header":     good[:8],
		"a response":       withFlags(0x8100),
		"an inverse query": withFlags(0x0900),
		"two questions":    twoQuestions,
		"cut question":     good[:len(good)-3],
		"dot in a label":   append(append(append([]byte{}, good[:12]...), 7, 'a', '.', 'b', 'c', 'd', 'e', 'f', 0), good[len(good)-4:]...),
		"loop":             append(append([]byte{}, good[:12]...), 0xc0, 12, 0, 1, 0, 1),
	} {
		if _, err := ParseQuery(msg); err == nil {
			t.Errorf("ParseQuery(%s) succeeded", label)
		}
	}
}

func TestParseAnswerReadsAddressesThroughAliases(t *testing.T) {
	q := query(9, "www.example.com", TypeA)
	got, err := ParseAnswer(response(q, 0, cname(60, "edge.example.net"), a(30, "203.0.113.5"), a(30, "203.0.113.6"), a(30, "2001:db8::5")))
	if err != nil {
		t.Fatal(err)
	}
	want := Answer{Records: []Record{
		{Name: "www.example.com", Addr: netip.MustParseAddr("203.0.113.5"), TTL: 30},
		{Name: "www.example.com", Addr: netip.MustParseAddr("203.0.113.6"), TTL: 30},
		{Name: "www.example.com", Addr: netip.MustParseAddr("2001:db8::5"), TTL: 30},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseAnswer = %+v, want %+v", got, want)
	}
}

func TestParseAnswerReportsCodeAndTruncation(t *testing.T) {
	q := query(9, "example.com", TypeA)
	nx := response(q, 3)
	if got, err := ParseAnswer(nx); err != nil || got.Rcode != 3 || len(got.Records) != 0 {
		t.Errorf("ParseAnswer(NXDOMAIN) = %+v, %v", got, err)
	}
	cut := response(q, 0)
	cut[2] |= 0x02
	if got, err := ParseAnswer(cut); err != nil || !got.Truncated {
		t.Errorf("ParseAnswer(truncated) = %+v, %v", got, err)
	}
	if _, err := ParseAnswer(q); err == nil {
		t.Error("ParseAnswer accepted a query")
	}
	if _, err := ParseAnswer(response(q, 0, a(1, "203.0.113.5"))[:40]); err == nil {
		t.Error("ParseAnswer accepted a cut record")
	}
}

func TestReplyEchoesTheQuestion(t *testing.T) {
	q := query(0x1234, "evil.example", TypeA)
	got := Reply(q, RcodeRefused)
	want := append([]byte{}, q...)
	want[2], want[3] = 0x81, 0x85
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Reply = % x, want % x", got, want)
	}
	if got := Reply(q[:5], RcodeServFail); got != nil {
		t.Errorf("Reply to a cut header = % x, want none", got)
	}
	cut := Reply(q[:14], RcodeFormErr)
	if len(cut) != 12 || cut[3] != 0x81 || cut[5] != 0 {
		t.Errorf("Reply to a cut question = % x, want a bare header with code 1", cut)
	}
}

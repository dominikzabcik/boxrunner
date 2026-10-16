// Package dnsfwd answers a class's name lookups with its egress policy:
// names the policy allows go to the box's resolver and the addresses in the
// answer are opened for the class, and every other name is refused.
package dnsfwd

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"slices"
	"strings"
)

// Response codes and types the forwarder uses.
const (
	RcodeSuccess  = 0
	RcodeFormErr  = 1
	RcodeServFail = 2
	RcodeRefused  = 5

	TypeA    = 1
	TypeAAAA = 28

	typeCNAME = 5
)

const (
	headerLen  = 12
	maxName    = 255
	maxPointer = 128
)

// Question is the one question of a query.
type Question struct {
	Name  string
	Type  uint16
	Class uint16
}

// Record is an address in an answer.
type Record struct {
	// Name is the record's owner, which names the host or one of the aliases
	// on the way to it.
	Name string
	Addr netip.Addr
	// TTL is in seconds.
	TTL uint32
}

var errShort = errors.New("dns: message too short")

// ParseQuery reads a standard query with exactly one question.
func ParseQuery(msg []byte) (Question, error) {
	if len(msg) < headerLen {
		return Question{}, errShort
	}
	flags := binary.BigEndian.Uint16(msg[2:4])
	switch {
	case flags&0x8000 != 0:
		return Question{}, errors.New("dns: not a query")
	case flags>>11&0xf != 0:
		return Question{}, errors.New("dns: not a standard query")
	case binary.BigEndian.Uint16(msg[4:6]) != 1:
		return Question{}, errors.New("dns: need exactly one question")
	}
	name, off, err := readName(msg, headerLen)
	if err != nil {
		return Question{}, err
	}
	if off+4 > len(msg) {
		return Question{}, errShort
	}
	return Question{Name: name, Type: binary.BigEndian.Uint16(msg[off:]), Class: binary.BigEndian.Uint16(msg[off+2:])}, nil
}

// Alias says that a name is another name's alias.
type Alias struct {
	Owner, Target string
}

// Answer is what a response says: the question it answers, and the addresses
// and aliases in its answer section.
type Answer struct {
	Question Question
	Records  []Record
	Aliases  []Alias
}

// Reachable are the addresses the question's name resolves to: those whose
// owner is the name itself or is reached from it through aliases. A response
// may carry others, such as a forged record for a name nobody asked about.
func (a Answer) Reachable() []Record {
	names := map[string]bool{a.Question.Name: true}
	for range len(a.Aliases) {
		grew := false
		for _, al := range a.Aliases {
			if names[al.Owner] && !names[al.Target] {
				names[al.Target], grew = true, true
			}
		}
		if !grew {
			break
		}
	}
	var out []Record
	for _, r := range a.Records {
		if names[r.Name] {
			out = append(out, r)
		}
	}
	return out
}

// ParseAnswer reads a response's question, address records and aliases.
func ParseAnswer(msg []byte) (Answer, error) {
	if len(msg) < headerLen {
		return Answer{}, errShort
	}
	flags := binary.BigEndian.Uint16(msg[2:4])
	if flags&0x8000 == 0 {
		return Answer{}, errors.New("dns: not a response")
	}
	var a Answer
	questions, answers := int(binary.BigEndian.Uint16(msg[4:6])), int(binary.BigEndian.Uint16(msg[6:8]))
	off := headerLen
	for i := range questions {
		name, next, err := readName(msg, off)
		if err != nil || next+4 > len(msg) {
			return Answer{}, errors.Join(err, errShort)
		}
		if i == 0 {
			a.Question = Question{Name: name, Type: binary.BigEndian.Uint16(msg[next:]), Class: binary.BigEndian.Uint16(msg[next+2:])}
		}
		off = next + 4
	}
	for range answers {
		name, next, err := readName(msg, off)
		if err != nil {
			return a, err
		}
		if next+10 > len(msg) {
			return a, errShort
		}
		typ := binary.BigEndian.Uint16(msg[next:])
		ttl := binary.BigEndian.Uint32(msg[next+4:])
		rdlen := int(binary.BigEndian.Uint16(msg[next+8:]))
		data := next + 10
		if data+rdlen > len(msg) {
			return a, errShort
		}
		rdata := msg[data : data+rdlen]
		switch {
		case typ == typeCNAME:
			if target, _, err := readName(msg, data); err == nil {
				a.Aliases = append(a.Aliases, Alias{Owner: name, Target: target})
			}
		case typ == TypeA && rdlen == 4:
			a.Records = append(a.Records, Record{Name: name, Addr: netip.AddrFrom4([4]byte(rdata)), TTL: ttl})
		case typ == TypeAAAA && rdlen == 16:
			a.Records = append(a.Records, Record{Name: name, Addr: netip.AddrFrom16([16]byte(rdata)), TTL: ttl})
		}
		off = data + rdlen
	}
	return a, nil
}

// Reply answers a query with a code and no records, echoing its question.
func Reply(query []byte, rcode int) []byte {
	if len(query) < headerLen {
		return nil
	}
	end := headerLen
	if _, off, err := readName(query, headerLen); err == nil && off+4 <= len(query) {
		end = off + 4
	}
	out := make([]byte, end)
	copy(out, query[:end])
	flags := binary.BigEndian.Uint16(query[2:4])
	// A response, with the query's opcode and recursion bit, recursion available.
	binary.BigEndian.PutUint16(out[2:], 0x8000|flags&0x7900|0x0080|uint16(rcode))
	binary.BigEndian.PutUint16(out[6:], 0)
	binary.BigEndian.PutUint16(out[8:], 0)
	binary.BigEndian.PutUint16(out[10:], 0)
	if end == headerLen {
		binary.BigEndian.PutUint16(out[4:], 0)
	}
	return out
}

// readName reads a possibly compressed name that starts at off, and returns
// it in lower case without the final dot, and the offset after it.
func readName(msg []byte, off int) (string, int, error) {
	var name []byte
	next, jumped := 0, false
	for hops := 0; ; {
		if off >= len(msg) {
			return "", 0, errShort
		}
		n := int(msg[off])
		switch {
		case n == 0:
			if !jumped {
				next = off + 1
			}
			return strings.ToLower(string(name)), next, nil
		case n&0xc0 == 0xc0:
			if off+1 >= len(msg) {
				return "", 0, errShort
			}
			if !jumped {
				next = off + 2
			}
			jumped = true
			if hops++; hops > maxPointer {
				return "", 0, errors.New("dns: compression loop")
			}
			off = int(binary.BigEndian.Uint16(msg[off:]) & 0x3fff)
		case n&0xc0 != 0:
			return "", 0, errors.New("dns: bad label")
		default:
			if off+1+n > len(msg) {
				return "", 0, errShort
			}
			for _, c := range msg[off+1 : off+1+n] {
				if !nameByte(c) {
					return "", 0, errors.New("dns: unusual byte in a label")
				}
			}
			if len(name) > 0 {
				name = append(name, '.')
			}
			name = append(name, msg[off+1:off+1+n]...)
			if len(name) > maxName {
				return "", 0, errors.New("dns: name too long")
			}
			off += 1 + n
		}
	}
}

// nameByte allows the letters, digits and dashes of host names and the
// underscore of service names.
func nameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
}

const typeOPT = 41

// ClampTTLs returns the response with no TTL above max seconds. A job that
// caches an answer for as long as its TTL says would otherwise keep using an
// address after the forwarder closed it.
func ClampTTLs(msg []byte, max uint32) []byte {
	out := slices.Clone(msg)
	if len(out) < headerLen {
		return out
	}
	off := headerLen
	for range int(binary.BigEndian.Uint16(out[4:6])) {
		_, next, err := readName(out, off)
		if err != nil || next+4 > len(out) {
			return out
		}
		off = next + 4
	}
	records := int(binary.BigEndian.Uint16(out[6:8])) + int(binary.BigEndian.Uint16(out[8:10])) + int(binary.BigEndian.Uint16(out[10:12]))
	for range records {
		_, next, err := readName(out, off)
		if err != nil || next+10 > len(out) {
			return out
		}
		// An OPT record's TTL field holds flags, not a time.
		if binary.BigEndian.Uint16(out[next:]) != typeOPT && binary.BigEndian.Uint32(out[next+4:]) > max {
			binary.BigEndian.PutUint32(out[next+4:], max)
		}
		off = next + 10 + int(binary.BigEndian.Uint16(out[next+8:]))
	}
	return out
}

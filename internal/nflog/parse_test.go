package nflog

import (
	"encoding/binary"
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

func nlattr(typ uint16, data []byte) []byte {
	b := binary.NativeEndian.AppendUint16(nil, uint16(4+len(data)))
	b = binary.NativeEndian.AppendUint16(b, typ)
	b = append(b, data...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

func message(typ uint16, family byte, attrs ...[]byte) []byte {
	b := make([]byte, 16, 64)
	binary.NativeEndian.PutUint16(b[4:], typ)
	b = append(b, family, 0, 0, 1)
	for _, a := range attrs {
		b = append(b, a...)
	}
	binary.NativeEndian.PutUint32(b, uint32(len(b)))
	return b
}

func ipv4(proto byte, src, dst string, sport, dport uint16) []byte {
	b := make([]byte, 20, 28)
	b[0], b[9] = 0x45, proto
	copy(b[12:], netip.MustParseAddr(src).AsSlice())
	copy(b[16:], netip.MustParseAddr(dst).AsSlice())
	b = binary.BigEndian.AppendUint16(b, sport)
	return binary.BigEndian.AppendUint16(b, dport)
}

func ipv6(proto byte, src, dst string, sport, dport uint16) []byte {
	b := make([]byte, 40, 48)
	b[0], b[6] = 0x60, proto
	copy(b[8:], netip.MustParseAddr(src).AsSlice())
	copy(b[24:], netip.MustParseAddr(dst).AsSlice())
	b = binary.BigEndian.AppendUint16(b, sport)
	return binary.BigEndian.AppendUint16(b, dport)
}

func logged(family byte, prefix string, ip []byte) []byte {
	return message(subsysULog<<8|msgPacket, family,
		nlattr(attrPacketHdr, []byte{8, 0, 2, 0}), nlattr(attrPrefix, []byte(prefix+"\x00")), nlattr(attrPayload, ip))
}

func TestParseReadsLoggedPackets(t *testing.T) {
	datagram := append(
		logged(2, "cd4f0b", ipv4(6, "172.19.0.2", "93.184.216.34", 40000, 443)),
		logged(10, "cd4f0b", ipv6(17, "fd00::2", "2001:db8::53", 5353, 53))...)
	got, err := Parse(datagram)
	if err != nil {
		t.Fatal(err)
	}
	want := []Packet{
		{Prefix: "cd4f0b", Src: netip.MustParseAddr("172.19.0.2"), Dst: netip.MustParseAddr("93.184.216.34"), Proto: 6, DstPort: 443},
		{Prefix: "cd4f0b", Src: netip.MustParseAddr("fd00::2"), Dst: netip.MustParseAddr("2001:db8::53"), Proto: 17, DstPort: 53},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Parse = %+v, want %+v", got, want)
	}
}

func TestParseKeepsIPWithoutPortsForOtherProtocols(t *testing.T) {
	got, err := Parse(logged(2, "cd4f0b", ipv4(1, "172.19.0.2", "198.51.100.1", 0x0800, 0)))
	if err != nil || len(got) != 1 || got[0].Proto != 1 || got[0].DstPort != 0 {
		t.Errorf("Parse(ICMP) = %+v, %v", got, err)
	}
}

// errorMessage is the kernel's reply to a request: 0 acknowledges it.
func errorMessage(errno int32) []byte {
	b := binary.NativeEndian.AppendUint32(message(nlmsgError, 0)[:16], uint32(errno))
	binary.NativeEndian.PutUint32(b, uint32(len(b)))
	return b
}

func TestParseReportsTheKernelsRefusal(t *testing.T) {
	if _, err := Parse(errorMessage(-1)); err == nil || !strings.Contains(err.Error(), "operation not permitted") {
		t.Errorf("Parse(refusal) = %v", err)
	}
	if _, err := Parse(errorMessage(0)); err != nil {
		t.Errorf("Parse(ack) = %v", err)
	}
}

func TestParseSkipsWhatItCannotRead(t *testing.T) {
	good := logged(2, "x", ipv4(6, "10.0.0.2", "10.0.0.3", 1, 2))
	bad := message(subsysULog<<8|msgPacket, 2, nlattr(attrPayload, []byte{1, 2, 3}))
	got, err := Parse(append(bad, good...))
	if err != nil || len(got) != 1 {
		t.Errorf("Parse = %+v, %v; want the readable packet", got, err)
	}
	if _, err := Parse(good[:len(good)-3]); err == nil {
		t.Error("Parse accepted a cut-off message")
	}
}

// Package nflog reads the packets that nftables logs to an NFLOG group. The
// egress rules log what they drop there, so the agent can say where a job
// tried to go. It reads the netfilter netlink socket itself: the kernel's
// message format is short, and a library would be the agent's first.
package nflog

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"syscall"
)

// Netlink and nfnetlink constants, from linux/netlink.h and
// linux/netfilter/nfnetlink_log.h.
const (
	nlmsgError = 2
	nlmsgDone  = 3
	subsysULog = 4

	msgPacket = 0
	msgConfig = 1

	attrPacketHdr = 1
	attrPayload   = 9
	attrPrefix    = 10

	headerLen = 16 + 4 // nlmsghdr and nfgenmsg
)

// Packet is a packet an nftables rule logged.
type Packet struct {
	// Prefix is the rule's log prefix.
	Prefix   string
	Src, Dst netip.Addr
	// Proto is the IP protocol number: 6 for TCP, 17 for UDP.
	Proto            uint8
	SrcPort, DstPort uint16
}

// Parse reads the packets in one datagram of netlink messages.
func Parse(datagram []byte) ([]Packet, error) {
	var packets []Packet
	for len(datagram) >= 16 {
		length := int(binary.NativeEndian.Uint32(datagram))
		if length < 16 || length > len(datagram) {
			return packets, errors.New("nflog: cut-off netlink message")
		}
		typ := binary.NativeEndian.Uint16(datagram[4:])
		msg := datagram[:length]
		next := min((length+3)&^3, len(datagram))
		datagram = datagram[next:]
		switch {
		case typ == nlmsgDone:
			return packets, nil
		case typ == nlmsgError:
			if length >= 20 {
				if errno := int32(binary.NativeEndian.Uint32(msg[16:])); errno != 0 {
					return packets, fmt.Errorf("nflog: the kernel refused: %w", syscall.Errno(-errno))
				}
			}
		case typ == subsysULog<<8|msgPacket:
			if p, ok := parsePacket(msg); ok {
				packets = append(packets, p)
			}
		}
	}
	return packets, nil
}

func parsePacket(msg []byte) (Packet, bool) {
	if len(msg) < headerLen {
		return Packet{}, false
	}
	var p Packet
	var payload []byte
	for attrs := msg[headerLen:]; len(attrs) >= 4; {
		length := int(binary.NativeEndian.Uint16(attrs))
		typ := binary.NativeEndian.Uint16(attrs[2:]) & 0x7fff
		if length < 4 || length > len(attrs) {
			break
		}
		data := attrs[4:length]
		switch typ {
		case attrPayload:
			payload = data
		case attrPrefix:
			p.Prefix = strings.TrimRight(string(data), "\x00")
		}
		attrs = attrs[min((length+3)&^3, len(attrs)):]
	}
	return parseIP(p, payload)
}

// parseIP reads the addresses and ports of a logged packet's headers.
func parseIP(p Packet, ip []byte) (Packet, bool) {
	var transport []byte
	switch {
	case len(ip) >= 20 && ip[0]>>4 == 4:
		ihl := int(ip[0]&0xf) * 4
		if ihl < 20 || len(ip) < ihl {
			return Packet{}, false
		}
		p.Src, p.Dst, p.Proto = netip.AddrFrom4([4]byte(ip[12:16])), netip.AddrFrom4([4]byte(ip[16:20])), ip[9]
		transport = ip[ihl:]
	case len(ip) >= 40 && ip[0]>>4 == 6:
		p.Src, p.Dst, p.Proto = netip.AddrFrom16([16]byte(ip[8:24])), netip.AddrFrom16([16]byte(ip[24:40])), ip[6]
		transport = ip[40:]
	default:
		return Packet{}, false
	}
	if (p.Proto == 6 || p.Proto == 17) && len(transport) >= 4 {
		p.SrcPort, p.DstPort = binary.BigEndian.Uint16(transport), binary.BigEndian.Uint16(transport[2:])
	}
	return p, true
}

package nflog

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"syscall"
)

const (
	netlinkNetfilter = 12
	nlmRequest       = 1
	nlmAck           = 4

	cfgCmd   = 1
	cfgMode  = 2
	cmdBind  = 1
	copyPkt  = 2
	copySize = 96
)

// Reader receives the packets logged to one group.
type Reader struct {
	file *os.File
	buf  []byte
}

// Open binds to a group. Only one process can hold a group, and it needs
// the right to open netlink sockets, which root has.
func Open(group uint16) (*Reader, error) {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, netlinkNetfilter)
	if err != nil {
		return nil, fmt.Errorf("nflog: %w", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("nflog: %w", err)
	}
	// A larger receive buffer rides out a burst of logged packets.
	syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, 1<<20)
	if err := configure(fd, group); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	if err := syscall.SetNonblock(fd, true); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("nflog: %w", err)
	}
	return &Reader{file: os.NewFile(uintptr(fd), "nflog"), buf: make([]byte, 1<<16)}, nil
}

// configure binds the group and asks for each packet's first bytes, and
// waits for the kernel to acknowledge both.
func configure(fd int, group uint16) error {
	bind := attr(cfgCmd, []byte{cmdBind})
	mode := binary.BigEndian.AppendUint32(nil, copySize)
	mode = append(mode, copyPkt, 0)
	for i, body := range [][]byte{bind, attr(cfgMode, mode)} {
		if err := syscall.Sendto(fd, config(group, uint32(i+1), body), 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
			return fmt.Errorf("nflog: %w", err)
		}
		reply := make([]byte, 4096)
		n, _, err := syscall.Recvfrom(fd, reply, 0)
		if err != nil {
			return fmt.Errorf("nflog: %w", err)
		}
		if _, err := Parse(reply[:n]); err != nil {
			return fmt.Errorf("binding group %d: %w", group, err)
		}
	}
	return nil
}

// config is a NFULNL_MSG_CONFIG message for a group.
func config(group uint16, seq uint32, attrs []byte) []byte {
	msg := make([]byte, headerLen, headerLen+len(attrs))
	binary.NativeEndian.PutUint32(msg[8:], seq)
	binary.NativeEndian.PutUint16(msg[4:], subsysULog<<8|msgConfig)
	binary.NativeEndian.PutUint16(msg[6:], nlmRequest|nlmAck)
	msg[16] = syscall.AF_UNSPEC
	binary.BigEndian.PutUint16(msg[18:], group)
	msg = append(msg, attrs...)
	binary.NativeEndian.PutUint32(msg, uint32(len(msg)))
	return msg
}

// attr is a netlink attribute, padded to four bytes.
func attr(typ uint16, data []byte) []byte {
	b := make([]byte, 4, 4+len(data)+3)
	binary.NativeEndian.PutUint16(b, uint16(4+len(data)))
	binary.NativeEndian.PutUint16(b[2:], typ)
	b = append(b, data...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

// Read waits for the next logged packets. A burst that overran the socket's
// buffer loses packets without an error.
func (r *Reader) Read() ([]Packet, error) {
	for {
		n, err := r.file.Read(r.buf)
		if err != nil {
			if errors.Is(err, syscall.ENOBUFS) {
				continue
			}
			return nil, err
		}
		if packets, _ := Parse(r.buf[:n]); len(packets) > 0 {
			return packets, nil
		}
	}
}

// Close stops reading, which ends a Read in progress.
func (r *Reader) Close() error { return r.file.Close() }

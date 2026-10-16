//go:build !linux

package nflog

import "errors"

// Reader receives the packets logged to one group; only Linux has them.
type Reader struct{}

// Open always fails off Linux.
func Open(uint16) (*Reader, error) {
	return nil, errors.New("nflog: only Linux logs packets to a group")
}

func (*Reader) Read() ([]Packet, error) { return nil, errors.New("nflog: unsupported") }

func (*Reader) Close() error { return nil }

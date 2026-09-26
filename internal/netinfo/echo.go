package netinfo

import (
	"crypto/rand"
	"encoding/binary"
)

// echoRequest builds an ICMP echo request whose payload starts with magic.
// The checksum is computed (raw sockets need it; ping sockets overwrite it).
func echoRequest(id, seq uint16, magic [8]byte) []byte {
	b := make([]byte, 8+16)
	b[0] = 8
	binary.BigEndian.PutUint16(b[4:], id)
	binary.BigEndian.PutUint16(b[6:], seq)
	copy(b[8:], magic[:])
	copy(b[16:], "fyisp-tr")
	binary.BigEndian.PutUint16(b[2:], checksum(b))
	return b
}

func checksum(b []byte) uint16 {
	var s uint32
	for i := 0; i+1 < len(b); i += 2 {
		s += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		s += uint32(b[len(b)-1]) << 8
	}
	for s>>16 != 0 {
		s = s&0xffff + s>>16
	}
	return ^uint16(s)
}

func newMagic() (m [8]byte) {
	_, _ = rand.Read(m[:])
	return m
}

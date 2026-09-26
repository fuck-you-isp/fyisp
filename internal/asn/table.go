package asn

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// On-disk format (all integers are unsigned LEB128 varints):
//
//	magic    "FYASN\x01"
//	version  len, bytes   build date of the table, e.g. "2026-09-26"
//	source   len, bytes   where the data came from (URL)
//	payload  one zstd frame (with checksum) holding, column by column:
//	  names    count, then len + bytes each        distinct AS names, sorted
//	  ASes     count, then asn deltas (>= 1)       AS numbers, ascending; AS index k+1
//	           count name indexes                  name of each AS
//	  entries  count, then start deltas            first start is 0, then strictly ascending
//	           count AS indexes (0 = unrouted)     each entry runs up to the next start - 1
//
// Entries cover the whole IPv4 space; adjacent entries never share an AS index.
// AS indexes are limited to 17 bits (131071 ASes; iptoasn lists about 80k).
const magic = "FYASN\x01"

const (
	maxHeaderString = 1024
	maxPayload      = 64 << 20
	// The zstd window bounds the decoder's history buffer; larger windows
	// barely help this data but cost memory on every load.
	maxWindow = 1 << 20
)

// table is a decoded table. Ranges are parallel uint32 slices and AS names
// are substrings of one string, so a loaded table is a handful of allocations.
type table struct {
	version, source string

	starts []uint32 // entry i covers starts[i] .. starts[i+1]-1
	asLo   []uint16 // AS index of entry i (low 16 bits); 0 = unrouted
	asHi   []uint64 // bit i: bit 16 of entry i's AS index (there are ~80k ASes)

	asns    []uint32 // by AS index; asns[0] = 0
	asName  []uint32 // by AS index: name index
	names   string   // all names, concatenated
	nameOff []uint32 // name n is names[nameOff[n]:nameOff[n+1]]
}

func (t *table) lookup(a [4]byte) Info {
	x := binary.BigEndian.Uint32(a[:])
	// First entry starting after x; the one before it contains x.
	lo, hi := 0, len(t.starts)
	for lo < hi {
		m := int(uint(lo+hi) >> 1)
		if t.starts[m] <= x {
			lo = m + 1
		} else {
			hi = m
		}
	}
	k := t.asIndex(lo - 1) // starts[0] == 0, so lo >= 1
	if k == 0 {
		return Info{}
	}
	n := t.asName[k]
	return Info{ASN: t.asns[k], Owner: t.names[t.nameOff[n]:t.nameOff[n+1]]}
}

func (t *table) asIndex(i int) uint32 {
	return uint32(t.asLo[i]) | uint32(t.asHi[i/64]>>(i%64)&1)<<16
}

var errCorrupt = errors.New("corrupt IP-to-ASN table")

type reader struct {
	b   []byte
	err error
}

func (r *reader) uvarint() uint64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Uvarint(r.b)
	if n <= 0 {
		r.err = errCorrupt
		return 0
	}
	r.b = r.b[n:]
	return v
}

// count reads a length that must be at most max and at most the bytes left
// (every counted item takes at least one byte), which bounds allocations.
func (r *reader) count(max uint64) int {
	v := r.uvarint()
	if r.err == nil && (v > max || v > uint64(len(r.b))) {
		r.err = errCorrupt
	}
	if r.err != nil {
		return 0
	}
	return int(v)
}

func (r *reader) bytes(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n > len(r.b) {
		r.err = errCorrupt
		return nil
	}
	b := r.b[:n]
	r.b = r.b[n:]
	return b
}

func decode(data []byte) (*table, error) {
	if !bytes.HasPrefix(data, []byte(magic)) {
		return nil, fmt.Errorf("%w: bad magic", errCorrupt)
	}
	r := &reader{b: data[len(magic):]}
	t := &table{}
	t.version = string(r.bytes(r.count(maxHeaderString)))
	t.source = string(r.bytes(r.count(maxHeaderString)))
	if r.err != nil {
		return nil, fmt.Errorf("%w: bad header", errCorrupt)
	}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true),
		zstd.WithDecoderMaxMemory(maxPayload), zstd.WithDecoderMaxWindow(maxWindow))
	if err != nil {
		return nil, err
	}
	payload, err := dec.DecodeAll(r.b, nil)
	dec.Close()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errCorrupt, err)
	}
	if err := t.decodePayload(payload); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *table) decodePayload(p []byte) error {
	r := &reader{b: p}

	nNames := r.count(1 << 24)
	t.nameOff = make([]uint32, nNames+1)
	// Two passes: offsets first, then one exactly sized string.
	names := r.b
	for i := 0; i < nNames && r.err == nil; i++ {
		n := len(r.bytes(r.count(maxHeaderString)))
		t.nameOff[i+1] = t.nameOff[i] + uint32(n)
	}
	if r.err == nil {
		var sb strings.Builder
		sb.Grow(int(t.nameOff[nNames]))
		nr := &reader{b: names}
		for i := 0; i < nNames; i++ {
			sb.Write(nr.bytes(nr.count(maxHeaderString)))
		}
		t.names = sb.String()
	}

	nAS := r.count(1 << 24)
	t.asns = make([]uint32, nAS+1)
	t.asName = make([]uint32, nAS+1)
	for k := 1; k <= nAS && r.err == nil; k++ {
		d := r.uvarint()
		v := uint64(t.asns[k-1]) + d
		if d == 0 || v > 1<<32-1 {
			return fmt.Errorf("%w: AS numbers not ascending", errCorrupt)
		}
		t.asns[k] = uint32(v)
	}
	for k := 1; k <= nAS && r.err == nil; k++ {
		n := r.uvarint()
		if n >= uint64(nNames) {
			return fmt.Errorf("%w: bad name index", errCorrupt)
		}
		t.asName[k] = uint32(n)
	}

	nEnt := r.count(1 << 32)
	if r.err == nil && nEnt == 0 {
		return fmt.Errorf("%w: no entries", errCorrupt)
	}
	t.starts = make([]uint32, nEnt)
	t.asLo = make([]uint16, nEnt)
	t.asHi = make([]uint64, (nEnt+63)/64)
	var prev uint64
	for i := 0; i < nEnt && r.err == nil; i++ {
		d := r.uvarint()
		v := prev + d
		if (i == 0 && d != 0) || (i > 0 && d == 0) || v > 1<<32-1 {
			return fmt.Errorf("%w: entry starts not ascending", errCorrupt)
		}
		t.starts[i], prev = uint32(v), v
	}
	for i := 0; i < nEnt && r.err == nil; i++ {
		k := r.uvarint()
		if k > uint64(nAS) || k >= 1<<17 {
			return fmt.Errorf("%w: bad AS index", errCorrupt)
		}
		t.asLo[i] = uint16(k)
		t.asHi[i/64] |= (k >> 16) << (i % 64)
	}
	if r.err != nil {
		return fmt.Errorf("%w: truncated payload", errCorrupt)
	}
	if len(r.b) != 0 {
		return fmt.Errorf("%w: trailing bytes", errCorrupt)
	}
	return nil
}

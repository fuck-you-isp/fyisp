package asn

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// BuildStats describes a table written by Build.
type BuildStats struct {
	Lines   int // input lines
	Routed  int // input ranges with an AS (ASN != 0)
	Entries int // table entries after merging (routed and unrouted)
	ASes    int // distinct AS numbers
	Names   int // distinct AS names
	Bytes   int // table size
}

type srcRange struct {
	start, end uint32
	asn        uint32
}

// Build reads iptoasn.com's ip2asn-v4.tsv (range_start, range_end, AS_number,
// country_code, AS_description; tab-separated) and writes the compact table
// format described in table.go. Ranges with AS 0 ("Not routed") are dropped
// and adjacent ranges of the same AS are merged. Only the generator and tests
// use it, so it is not linked into fyisp.
func Build(w io.Writer, r io.Reader, version, source string) (BuildStats, error) {
	var st BuildStats
	if len(version) > maxHeaderString || len(source) > maxHeaderString {
		return st, fmt.Errorf("version or source too long")
	}
	var ranges []srcRange
	nameOf := map[uint32]string{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		st.Lines++
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 3 {
			return st, fmt.Errorf("line %d: want at least 3 tab-separated fields, got %d", st.Lines, len(f))
		}
		a, err1 := netip.ParseAddr(f[0])
		b, err2 := netip.ParseAddr(f[1])
		n, err3 := strconv.ParseUint(f[2], 10, 32)
		if err1 != nil || err2 != nil || err3 != nil || !a.Is4() || !b.Is4() {
			return st, fmt.Errorf("line %d: bad range or AS number: %q", st.Lines, line)
		}
		s, e := be32(a), be32(b)
		if e < s {
			return st, fmt.Errorf("line %d: range end before start", st.Lines)
		}
		if n == 0 {
			continue
		}
		asn := uint32(n)
		name := ""
		if len(f) >= 5 {
			name = strings.TrimSpace(f[4])
		}
		if len(name) > maxHeaderString {
			name = name[:maxHeaderString]
		}
		if _, ok := nameOf[asn]; !ok {
			nameOf[asn] = name
		}
		ranges = append(ranges, srcRange{s, e, asn})
	}
	if err := sc.Err(); err != nil {
		return st, err
	}
	st.Routed = len(ranges)
	slices.SortFunc(ranges, func(x, y srcRange) int {
		switch {
		case x.start < y.start:
			return -1
		case x.start > y.start:
			return 1
		}
		return 0
	})
	for i := 1; i < len(ranges); i++ {
		if ranges[i].start <= ranges[i-1].end {
			return st, fmt.Errorf("overlapping ranges at %v", addr(ranges[i].start))
		}
	}

	// AS indexes follow AS number order (index 0 = unrouted); names are sorted.
	asns := make([]uint32, 0, len(nameOf))
	for a := range nameOf {
		asns = append(asns, a)
	}
	slices.Sort(asns)
	asIndex := make(map[uint32]uint64, len(asns))
	for i, a := range asns {
		asIndex[a] = uint64(i + 1)
	}
	names := make([]string, 0, len(asns))
	for _, a := range asns {
		names = append(names, nameOf[a])
	}
	slices.Sort(names)
	names = slices.Compact(names)
	nameIndex := make(map[string]uint64, len(names))
	for i, n := range names {
		nameIndex[n] = uint64(i)
	}

	// Entries: whole IPv4 space, starting with an unrouted entry at 0.
	type entry struct {
		start uint32
		as    uint64
	}
	ents := []entry{{0, 0}}
	add := func(start uint32, as uint64) {
		last := &ents[len(ents)-1]
		switch {
		case last.as == as:
		case last.start == start:
			last.as = as
			if len(ents) > 1 && ents[len(ents)-2].as == as {
				ents = ents[:len(ents)-1]
			}
		default:
			ents = append(ents, entry{start, as})
		}
	}
	for i, rg := range ranges {
		if i > 0 && rg.start > ranges[i-1].end+1 {
			add(ranges[i-1].end+1, 0)
		}
		add(rg.start, asIndex[rg.asn])
	}
	if n := len(ranges); n > 0 && ranges[n-1].end != 1<<32-1 {
		add(ranges[n-1].end+1, 0)
	}

	if len(asns) >= 1<<17 {
		return st, fmt.Errorf("%d ASes: the table format holds at most %d", len(asns), 1<<17-1)
	}

	var p []byte
	p = binary.AppendUvarint(p, uint64(len(names)))
	for _, n := range names {
		p = binary.AppendUvarint(p, uint64(len(n)))
		p = append(p, n...)
	}
	p = binary.AppendUvarint(p, uint64(len(asns)))
	prev := uint32(0)
	for _, a := range asns {
		p = binary.AppendUvarint(p, uint64(a-prev))
		prev = a
	}
	for _, a := range asns {
		p = binary.AppendUvarint(p, nameIndex[nameOf[a]])
	}
	p = binary.AppendUvarint(p, uint64(len(ents)))
	prev = 0
	for _, e := range ents {
		p = binary.AppendUvarint(p, uint64(e.start-prev))
		prev = e.start
	}
	for _, e := range ents {
		p = binary.AppendUvarint(p, e.as)
	}

	var out bytes.Buffer
	out.WriteString(magic)
	for _, s := range []string{version, source} {
		out.Write(binary.AppendUvarint(nil, uint64(len(s))))
		out.WriteString(s)
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression),
		zstd.WithWindowSize(maxWindow), zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(true))
	if err != nil {
		return st, err
	}
	out.Write(enc.EncodeAll(p, nil))
	enc.Close()
	st.Entries, st.ASes, st.Names, st.Bytes = len(ents), len(asns), len(names), out.Len()
	_, err = w.Write(out.Bytes())
	return st, err
}

func be32(a netip.Addr) uint32 { b := a.As4(); return binary.BigEndian.Uint32(b[:]) }

func addr(x uint32) netip.Addr {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], x)
	return netip.AddrFrom4(b)
}

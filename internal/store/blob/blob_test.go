package blob_test

import (
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"testing"

	"github.com/fuck-you-isp/fyisp/internal/store/blob"
	"github.com/fuck-you-isp/fyisp/internal/store/synth"
)

// randBlock builds a block exercising every slot kind and value extremes.
func randBlock(r *rand.Rand) blob.Block {
	b := blob.Block{
		Slot0:    r.Int64N(1 << 45),
		Interval: 1 + r.Int64N(blob.MaxInterval),
	}
	n := r.IntN(4000)
	if r.IntN(50) == 0 {
		n = blob.MaxSlots
	}
	for range n {
		switch k := r.IntN(10); {
		case k < 2:
			b.Slots = append(b.Slots, blob.Lost(uint8(r.IntN(blob.MaxCode+1)))) // includes code 0 = gap
		case k < 3:
			b.Slots = append(b.Slots, blob.Val([]int64{0, 1, blob.MaxValue, blob.MaxValue - 1}[r.IntN(4)]))
		case k < 4:
			b.Slots = append(b.Slots, blob.Val(r.Int64N(blob.MaxValue+1)))
		default:
			b.Slots = append(b.Slots, blob.Val(r.Int64N(20_000)))
		}
	}
	return b
}

func equal(t *testing.T, want, got blob.Block) {
	t.Helper()
	if want.Slot0 != got.Slot0 || want.Interval != got.Interval || len(want.Slots) != len(got.Slots) {
		t.Fatalf("header/len mismatch: want %d/%d/%d got %d/%d/%d",
			want.Slot0, want.Interval, len(want.Slots), got.Slot0, got.Interval, len(got.Slots))
	}
	for i := range want.Slots {
		w, g := want.Slots[i], got.Slots[i]
		if !w.Valid {
			w.Value = 0
		} else {
			w.Code = 0
		}
		if w != g {
			t.Fatalf("slot %d: want %+v got %+v", i, w, g)
		}
	}
}

func TestRoundTripProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	iters := 300
	if testing.Short() {
		iters = 30
	}
	for range iters {
		b := randBlock(r)
		enc, err := blob.Encode(&b)
		if err != nil {
			t.Fatal(err)
		}
		got, err := blob.Decode(enc)
		if err != nil {
			t.Fatal(err)
		}
		equal(t, b, got)
		raw, err := blob.AppendRaw(nil, &b)
		if err != nil {
			t.Fatal(err)
		}
		got, err = blob.DecodeRaw(raw)
		if err != nil {
			t.Fatal(err)
		}
		equal(t, b, got)
	}
}

func TestAllCodesAndGaps(t *testing.T) {
	b := blob.Block{Slot0: 1767225600000, Interval: 15000}
	for c := uint8(0); c <= blob.MaxCode; c++ {
		b.Slots = append(b.Slots, blob.Val(int64(c)*1000), blob.Lost(c), blob.Gap())
	}
	enc, err := blob.Encode(&b)
	if err != nil {
		t.Fatal(err)
	}
	got, err := blob.Decode(enc)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, b, got)
	// Lost slots must not reset the delta base: value after a loss is exact.
	if got.Slots[3].Value != 1000 {
		t.Fatalf("delta base reset: %+v", got.Slots[3])
	}
}

func TestEncodeRejects(t *testing.T) {
	for name, b := range map[string]blob.Block{
		"code":      {Interval: 1, Slots: []blob.Slot{blob.Lost(16)}},
		"negative":  {Interval: 1, Slots: []blob.Slot{blob.Val(-1)}},
		"too big":   {Interval: 1, Slots: []blob.Slot{blob.Val(blob.MaxValue + 1)}},
		"interval0": {Interval: 0},
		"interval":  {Interval: blob.MaxInterval + 1},
		"slot0":     {Slot0: -1, Interval: 1},
		"slots":     {Interval: 1, Slots: make([]blob.Slot, blob.MaxSlots+1)},
	} {
		if _, err := blob.Encode(&b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestUnknownVersion(t *testing.T) {
	b := blob.Block{Slot0: 5, Interval: 1000, Slots: []blob.Slot{blob.Val(1)}}
	raw, _ := blob.AppendRaw(nil, &b)
	for _, v := range []byte{0, 2, 255} {
		raw[0] = v
		_, err := blob.DecodeRaw(raw)
		var ve *blob.VersionError
		if !errors.As(err, &ve) || ve.Version != v {
			t.Fatalf("version %d: got %v", v, err)
		}
	}
	// Garbage that is not zstd is corrupt, not a version error.
	if _, err := blob.Decode([]byte{2, 1, 2, 3}); !errors.Is(err, blob.ErrCorrupt) {
		t.Fatalf("garbage: %v", err)
	}
}

func TestDecodeRejectsBadRaw(t *testing.T) {
	hdr := []byte{1, 5, 0x80, 0x01} // slot0=5, interval=128
	for name, raw := range map[string][]byte{
		"empty":        {},
		"no interval":  {1, 5},
		"interval 0":   {1, 5, 0},
		"code 16":      append(append([]byte{}, hdr...), 16<<1|1),
		"truncated":    append(append([]byte{}, hdr...), 0x80),
		"negative":     append(append([]byte{}, hdr...), 1<<1), // zigzag(-1)<<1 = 2
		"huge delta":   binary.AppendUvarint(append([]byte{}, hdr...), 1<<62),
		"varint 11 by": append(append([]byte{}, hdr...), 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01),
	} {
		if _, err := blob.DecodeRaw(raw); !errors.Is(err, blob.ErrCorrupt) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

// TestSyntheticSize reports compressed bytes per sample for a full hour at
// the intervals fyisp uses.
func TestSyntheticSize(t *testing.T) {
	for _, iv := range []int64{1000, 5000, 15000, 60000} {
		n := int(3600_000 / iv)
		var samples, rawB, encB int
		for id := range 90 {
			s := synth.New(42, id)
			b := blob.Block{Slot0: 1767225600000, Interval: iv, Slots: s.Fill(nil, n)}
			raw, _ := blob.AppendRaw(nil, &b)
			enc, err := blob.Encode(&b)
			if err != nil {
				t.Fatal(err)
			}
			samples += n
			rawB += len(raw)
			encB += len(enc)
		}
		t.Logf("interval %5dms: %4d slots/hour  raw %.3f B/sample  zstd %.3f B/sample  blob %.0f B",
			iv, n, float64(rawB)/float64(samples), float64(encB)/float64(samples), float64(encB)/90)
	}
}

func FuzzDecode(f *testing.F) {
	r := rand.New(rand.NewPCG(3, 4))
	for range 8 {
		b := randBlock(r)
		b.Slots = b.Slots[:min(len(b.Slots), 200)]
		enc, _ := blob.Encode(&b)
		f.Add(enc)
	}
	f.Add([]byte{})
	f.Add([]byte{0x28, 0xb5, 0x2f, 0xfd})
	f.Fuzz(func(t *testing.T, data []byte) {
		b, err := blob.Decode(data)
		if err != nil {
			return
		}
		// Anything that decodes must re-encode to an equivalent block.
		enc, err := blob.Encode(&b)
		if err != nil {
			t.Fatalf("decoded block does not re-encode: %v", err)
		}
		got, err := blob.Decode(enc)
		if err != nil {
			t.Fatal(err)
		}
		equal(t, b, got)
	})
}

func FuzzDecodeRaw(f *testing.F) {
	r := rand.New(rand.NewPCG(5, 6))
	for range 8 {
		b := randBlock(r)
		b.Slots = b.Slots[:min(len(b.Slots), 200)]
		raw, _ := blob.AppendRaw(nil, &b)
		f.Add(raw)
	}
	f.Add([]byte{1, 0, 1, 1, 3, 5, 31, 0x80, 0x01})
	f.Fuzz(func(t *testing.T, raw []byte) {
		b, err := blob.DecodeRaw(raw)
		if err != nil {
			var ve *blob.VersionError
			if !errors.Is(err, blob.ErrCorrupt) && !errors.As(err, &ve) {
				t.Fatalf("unexpected error type %T: %v", err, err)
			}
			return
		}
		// Canonical inputs re-encode byte-for-byte; non-canonical varints may not,
		// but must still round-trip the block.
		re, err := blob.AppendRaw(nil, &b)
		if err != nil {
			t.Fatalf("decoded block does not re-encode: %v", err)
		}
		got, err := blob.DecodeRaw(re)
		if err != nil {
			t.Fatal(err)
		}
		equal(t, b, got)
	})
}

// FuzzRoundTrip builds blocks from fuzz bytes and checks Encode/Decode.
func FuzzRoundTrip(f *testing.F) {
	f.Add(int64(1767225600000), int64(15000), []byte{0, 1, 2, 3, 0xff, 0x10})
	f.Fuzz(func(t *testing.T, slot0, iv int64, p []byte) {
		b := blob.Block{Slot0: slot0, Interval: iv}
		for len(p) >= 3 && len(b.Slots) < 5000 {
			x := int64(binary.LittleEndian.Uint16(p[1:3]))
			switch {
			case p[0] < 64:
				b.Slots = append(b.Slots, blob.Lost(p[0]&0x1f))
			case p[0] < 70:
				b.Slots = append(b.Slots, blob.Val(x<<24))
			default:
				b.Slots = append(b.Slots, blob.Val(x))
			}
			p = p[3:]
		}
		enc, err := blob.Encode(&b)
		if err != nil {
			return // invalid input rejected
		}
		got, err := blob.Decode(enc)
		if err != nil {
			t.Fatal(err)
		}
		equal(t, b, got)
	})
}

func BenchmarkDecodeHour15s(b *testing.B) {
	s := synth.New(1, 1)
	blk := blob.Block{Slot0: 1767225600000, Interval: 15000, Slots: s.Fill(nil, 240)}
	enc, _ := blob.Encode(&blk)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := blob.Decode(enc); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncodeHour1s(b *testing.B) {
	s := synth.New(1, 1)
	blk := blob.Block{Slot0: 1767225600000, Interval: 1000, Slots: s.Fill(nil, 3600)}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := blob.Encode(&blk); err != nil {
			b.Fatal(err)
		}
	}
}

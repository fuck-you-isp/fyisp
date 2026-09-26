// Package blob encodes one series' hour of probe results as a compact blob
// (format v1, see docs/PLAN.md "Storage").
//
// Raw layout, before compression:
//
//	byte     version (1)
//	uvarint  slot0 (unix ms of slot 0)
//	uvarint  interval (ms, > 0)
//	uvarint  one per slot, until the end of the data:
//	           u&1 == 0: value; u>>1 is the zigzag delta from the previous
//	                     value in 10 µs units (the first value is relative to 0)
//	           u&1 == 1: no value; code = u>>1 (0 = not measured, 1..15 = loss reason)
//
// The raw bytes are then zstd-compressed (SpeedBestCompression, no dictionary).
// Slots without a value do not reset the delta base.
package blob

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/klauspost/compress/zstd"
)

const (
	// Version1 is the only format this package writes.
	Version1 byte = 1
	// MaxCode is the largest loss reason code.
	MaxCode = 15
	// MaxSlots bounds the slots in one blob (an hour at 100 ms is 36000).
	MaxSlots = 1 << 16
	// MaxValue bounds a value in 10 µs units (about 127 days).
	MaxValue = 1 << 40
	// MaxInterval bounds the slot interval (one day, in ms).
	MaxInterval = 86_400_000
	// Unit is the value resolution in nanoseconds.
	Unit = 10_000

	maxRaw = 1 + 2*binary.MaxVarintLen64 + MaxSlots*binary.MaxVarintLen64
)

// Slot is one probe result.
type Slot struct {
	Value int64 // RTT in 10 µs units; meaningful only when Valid
	Valid bool  // a measured value
	Code  uint8 // when !Valid: 0 = not measured (gap), 1..15 = loss reason
}

// Val returns a slot holding a measured value (10 µs units).
func Val(v int64) Slot { return Slot{Value: v, Valid: true} }

// Gap returns a slot that was not measured.
func Gap() Slot { return Slot{} }

// Lost returns a slot lost for the given reason (1..15).
func Lost(code uint8) Slot { return Slot{Code: code} }

// Block is one series' slots starting at Slot0.
type Block struct {
	Slot0    int64 // unix ms of slot 0
	Interval int64 // ms between slots
	Slots    []Slot
}

// VersionError reports a blob written in a format this build cannot read.
type VersionError struct{ Version byte }

func (e *VersionError) Error() string { return fmt.Sprintf("blob: unknown version %d", e.Version) }

// ErrCorrupt is wrapped by every decode error other than *VersionError.
var ErrCorrupt = errors.New("blob: corrupt")

func corrupt(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrCorrupt}, a...)...)
}

var (
	enc *zstd.Encoder
	dec *zstd.Decoder
)

func init() {
	var err error
	// EncodeAll/DecodeAll are safe for concurrent use.
	enc, err = zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedBestCompression),
		zstd.WithEncoderCRC(true),
		zstd.WithEncoderConcurrency(1),
		zstd.WithLowerEncoderMem(true),
		zstd.WithWindowSize(1<<20))
	if err != nil {
		panic(err)
	}
	dec, err = zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(0),
		zstd.WithDecoderLowmem(true),
		zstd.WithDecoderMaxMemory(maxRaw),
		zstd.WithDecoderMaxWindow(1<<20))
	if err != nil {
		panic(err)
	}
}

func zz(x int64) uint64   { return uint64(x<<1) ^ uint64(x>>63) }
func unzz(u uint64) int64 { return int64(u>>1) ^ -int64(u&1) }

func (b *Block) validate() error {
	switch {
	case b.Slot0 < 0:
		return fmt.Errorf("blob: negative slot0 %d", b.Slot0)
	case b.Interval <= 0 || b.Interval > MaxInterval:
		return fmt.Errorf("blob: interval %d out of range", b.Interval)
	case len(b.Slots) > MaxSlots:
		return fmt.Errorf("blob: %d slots > %d", len(b.Slots), MaxSlots)
	}
	return nil
}

// AppendRaw appends the uncompressed v1 encoding of b to dst.
func AppendRaw(dst []byte, b *Block) ([]byte, error) {
	if err := b.validate(); err != nil {
		return dst, err
	}
	dst = append(dst, Version1)
	dst = binary.AppendUvarint(dst, uint64(b.Slot0))
	dst = binary.AppendUvarint(dst, uint64(b.Interval))
	var prev int64
	for i, s := range b.Slots {
		if !s.Valid {
			if s.Code > MaxCode {
				return dst, fmt.Errorf("blob: slot %d: code %d > %d", i, s.Code, MaxCode)
			}
			dst = append(dst, s.Code<<1|1)
			continue
		}
		if s.Value < 0 || s.Value > MaxValue {
			return dst, fmt.Errorf("blob: slot %d: value %d out of range", i, s.Value)
		}
		dst = binary.AppendUvarint(dst, zz(s.Value-prev)<<1)
		prev = s.Value
	}
	return dst, nil
}

// Encode returns the compressed v1 blob for b.
func Encode(b *Block) ([]byte, error) {
	raw, err := AppendRaw(make([]byte, 0, 32+2*len(b.Slots)), b)
	if err != nil {
		return nil, err
	}
	return enc.EncodeAll(raw, make([]byte, 0, len(raw)/2+32)), nil
}

// Decode decompresses and decodes a blob written by Encode. It never panics;
// errors are either *VersionError or wrap ErrCorrupt.
func Decode(data []byte) (Block, error) {
	raw, err := dec.DecodeAll(data, nil)
	if err != nil {
		return Block{}, corrupt("zstd: %v", err)
	}
	return DecodeRaw(raw)
}

// DecodeRaw decodes the uncompressed encoding produced by AppendRaw.
func DecodeRaw(raw []byte) (Block, error) {
	if len(raw) == 0 {
		return Block{}, corrupt("empty")
	}
	if raw[0] != Version1 {
		return Block{}, &VersionError{raw[0]}
	}
	p := raw[1:]
	slot0, n := binary.Uvarint(p)
	if n <= 0 || slot0 > 1<<62 {
		return Block{}, corrupt("bad slot0")
	}
	p = p[n:]
	iv, n := binary.Uvarint(p)
	if n <= 0 || iv == 0 || iv > MaxInterval {
		return Block{}, corrupt("bad interval")
	}
	p = p[n:]
	b := Block{Slot0: int64(slot0), Interval: int64(iv)}
	// Every slot takes at least one byte, so len(p) bounds the count.
	if len(p) > MaxSlots*binary.MaxVarintLen64 {
		return Block{}, corrupt("too long")
	}
	b.Slots = make([]Slot, 0, min(len(p), MaxSlots))
	var prev int64
	for len(p) > 0 {
		if len(b.Slots) == MaxSlots {
			return Block{}, corrupt("more than %d slots", MaxSlots)
		}
		u, n := binary.Uvarint(p)
		if n <= 0 {
			return Block{}, corrupt("bad varint at slot %d", len(b.Slots))
		}
		p = p[n:]
		if u&1 == 1 {
			c := u >> 1
			if c > MaxCode {
				return Block{}, corrupt("code %d at slot %d", c, len(b.Slots))
			}
			b.Slots = append(b.Slots, Slot{Code: uint8(c)})
			continue
		}
		d := unzz(u >> 1)
		if d < -MaxValue || d > MaxValue {
			return Block{}, corrupt("delta out of range at slot %d", len(b.Slots))
		}
		v := prev + d
		if v < 0 || v > MaxValue {
			return Block{}, corrupt("value out of range at slot %d", len(b.Slots))
		}
		b.Slots = append(b.Slots, Slot{Value: v, Valid: true})
		prev = v
	}
	return b, nil
}

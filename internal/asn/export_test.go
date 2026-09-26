package asn

import (
	"testing"

	"github.com/klauspost/compress/zstd"
)

func zstdCompress(t *testing.T, p []byte) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderCRC(true))
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	return enc.EncodeAll(p, nil)
}

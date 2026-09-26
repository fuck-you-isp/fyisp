package asn

import (
	"bytes"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Golden lookups against the built-in table.
func TestLookupEmbedded(t *testing.T) {
	for _, c := range []struct {
		ip     string
		asn    uint32
		prefix string // expected Owner prefix
	}{
		{"1.1.1.1", 13335, "CLOUDFLARENET"},
		{"1.0.0.1", 13335, "CLOUDFLARENET"},
		{"::ffff:1.1.1.1", 13335, "CLOUDFLARENET"},
		{"8.8.8.8", 15169, "GOOGLE"},
		{"8.8.4.4", 15169, "GOOGLE"},
		{"9.9.9.9", 19281, "QUAD9"},
		{"10.0.0.1", 0, ""},
		{"172.16.5.4", 0, ""},
		{"192.168.1.1", 0, ""},
		{"127.0.0.1", 0, ""},
		{"169.254.1.1", 0, ""},
		{"100.64.0.1", 0, ""},
		{"0.0.0.0", 0, ""},
		{"224.0.0.1", 0, ""},
		{"255.255.255.255", 0, ""},
		{"2606:4700:4700::1111", 0, ""}, // IPv6: not covered
	} {
		got := Lookup(netip.MustParseAddr(c.ip))
		if got.ASN != c.asn || !strings.HasPrefix(got.Owner, c.prefix) || (c.asn == 0 && got.Owner != "") {
			t.Errorf("Lookup(%s) = %+v, want ASN %d owner %q...", c.ip, got, c.asn, c.prefix)
		}
	}
	if got := Lookup(netip.Addr{}); got != (Info{}) {
		t.Errorf("Lookup(zero Addr) = %+v", got)
	}
}

func TestVersion(t *testing.T) {
	v := Version()
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`).MatchString(v) {
		t.Fatalf("Version() = %q, want YYYY-MM-DD", v)
	}
	if _, err := time.Parse(time.DateOnly, v); err != nil {
		t.Fatalf("Version() = %q: %v", v, err)
	}
	if tb := std.get(); !strings.HasPrefix(tb.source, "https://iptoasn.com/") {
		t.Errorf("source = %q", tb.source)
	}
}

// The embedded table must be well-formed and large enough to be real.
func TestEmbeddedShape(t *testing.T) {
	tb := std.get()
	if tb == nil {
		t.Fatal("embedded table did not decode")
	}
	if len(tb.starts) < 100000 || len(tb.asns) < 10000 {
		t.Fatalf("suspiciously small table: %d entries, %d ASes", len(tb.starts), len(tb.asns))
	}
	for i := 1; i < len(tb.starts); i++ {
		if tb.asIndex(i) == tb.asIndex(i-1) {
			t.Fatalf("entries %d and %d not merged", i-1, i)
		}
	}
}

const smallTSV = "" +
	"0.0.0.0\t0.0.0.255\t64513\tXX\tZERO-NET\n" +
	"1.0.0.0\t1.0.0.255\t13335\tUS\tCLOUDFLARENET\n" +
	"1.0.1.0\t1.0.3.255\t0\tNone\tNot routed\n" +
	"1.0.8.0\t1.0.8.255\t38803\tAU\tGTELECOM\n" + // unsorted on purpose
	"1.0.4.0\t1.0.7.255\t38803\tAU\tGTELECOM\n" +
	"1.0.9.0\t1.0.9.127\t38803\tAU\tGTELECOM\n" +
	"1.0.9.128\t1.0.9.128\t65001\tXX\tSHARED-NAME\n" +
	"1.0.9.129\t1.0.9.255\t65000\tXX\tSHARED-NAME\n" +
	"1.0.11.0\t1.0.11.255\t65000\tXX\tSHARED-NAME\n" +
	"255.255.255.0\t255.255.255.255\t64512\tXX\tTOP-NET\n"

func buildSmall(t testing.TB, tsv string) []byte {
	t.Helper()
	var b bytes.Buffer
	if _, err := Build(&b, strings.NewReader(tsv), "2026-01-02", "test"); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestSmallTableEdges(t *testing.T) {
	data := buildSmall(t, smallTSV)
	tb, err := decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if tb.version != "2026-01-02" || tb.source != "test" {
		t.Errorf("header = %q %q", tb.version, tb.source)
	}
	cf := Info{13335, "CLOUDFLARENET"}
	gt := Info{38803, "GTELECOM"}
	for _, c := range []struct {
		ip   string
		want Info
	}{
		{"0.0.0.0", Info{64513, "ZERO-NET"}}, // range starting at 0
		{"0.0.0.255", Info{64513, "ZERO-NET"}},
		{"0.0.1.0", Info{}},
		{"0.255.255.255", Info{}},
		{"1.0.0.0", cf},
		{"1.0.0.255", cf},
		{"1.0.1.0", Info{}}, // "Not routed" dropped
		{"1.0.3.255", Info{}},
		{"1.0.4.0", gt},
		{"1.0.8.255", gt}, // merged with 1.0.4.0/22
		{"1.0.9.127", gt},
		{"1.0.9.128", Info{65001, "SHARED-NAME"}},
		{"1.0.9.129", Info{65000, "SHARED-NAME"}},
		{"1.0.9.255", Info{65000, "SHARED-NAME"}},
		{"1.0.10.0", Info{}}, // gap
		{"1.0.10.255", Info{}},
		{"1.0.11.0", Info{65000, "SHARED-NAME"}},
		{"1.0.12.0", Info{}},
		{"255.255.254.255", Info{}},
		{"255.255.255.0", Info{64512, "TOP-NET"}}, // range ending at the top
		{"255.255.255.255", Info{64512, "TOP-NET"}},
	} {
		if got := tb.lookup(netip.MustParseAddr(c.ip).As4()); got != c.want {
			t.Errorf("lookup(%s) = %+v, want %+v", c.ip, got, c.want)
		}
	}
	// 0/24, gap, cf, gap, gtelecom (merged: 3 ranges -> 1), 65001, 65000, gap, 65000, gap, top.
	if len(tb.starts) != 11 {
		t.Errorf("entries = %d, want 11: %v", len(tb.starts), tb.starts)
	}
	if len(tb.nameOff)-1 != 5 || len(tb.asns)-1 != 6 {
		t.Errorf("names = %d, ASes = %d; want 5, 6", len(tb.nameOff)-1, len(tb.asns)-1)
	}
	// Through db: 255.255.255.255 is reserved, so the public filter hides it.
	d := newDB(data, nil)
	if got := d.lookup(netip.MustParseAddr("255.255.255.255")); got != (Info{}) {
		t.Errorf("db.lookup(broadcast) = %+v", got)
	}
	if got := d.lookup(netip.MustParseAddr("1.0.0.7")); got != cf {
		t.Errorf("db.lookup(1.0.0.7) = %+v", got)
	}
}

// More than 65535 ASes exercises the 17th bit of the AS index.
func TestManyASes(t *testing.T) {
	const n = 70000
	var sb strings.Builder
	for i := 0; i < n; i++ {
		a := addr(uint32(0x01000000 + 512*i)) // one /24 routed, one /24 gap
		b := addr(uint32(0x01000000 + 512*i + 255))
		fmt.Fprintf(&sb, "%s\t%s\t%d\tXX\tNET-%d\n", a, b, 100000+i, i)
	}
	tb, err := decode(buildSmall(t, sb.String()))
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{0, 1, 65534, 65535, 65536, 65537, n - 1} {
		want := Info{uint32(100000 + i), fmt.Sprintf("NET-%d", i)}
		if got := tb.lookup(addr(uint32(0x01000000 + 512*i + 7)).As4()); got != want {
			t.Errorf("AS #%d: got %+v, want %+v", i, got, want)
		}
		if got := tb.lookup(addr(uint32(0x01000000 + 512*i + 256)).As4()); got != (Info{}) {
			t.Errorf("gap after AS #%d: got %+v", i, got)
		}
	}
}

func TestBuildEmptyAndErrors(t *testing.T) {
	tb, err := decode(buildSmall(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if got := tb.lookup([4]byte{1, 2, 3, 4}); got != (Info{}) {
		t.Errorf("empty table lookup = %+v", got)
	}
	for name, tsv := range map[string]string{
		"overlap":   "1.0.0.0\t1.0.0.255\t1\tXX\tA\n1.0.0.128\t1.0.1.255\t2\tXX\tB\n",
		"reversed":  "1.0.0.9\t1.0.0.1\t1\tXX\tA\n",
		"bad asn":   "1.0.0.0\t1.0.0.255\tAS1\tXX\tA\n",
		"ipv6":      "::1\t::2\t1\tXX\tA\n",
		"too short": "1.0.0.0\t1.0.0.255\n",
	} {
		if _, err := Build(new(bytes.Buffer), strings.NewReader(tsv), "v", "s"); err == nil {
			t.Errorf("%s: Build succeeded", name)
		}
	}
}

func TestCorruptTables(t *testing.T) {
	good := buildSmall(t, smallTSV)
	bad := map[string][]byte{
		"empty":     nil,
		"bad magic": append([]byte("XXXXX\x01"), good[len(magic):]...),
		"trailing":  append(append([]byte{}, good...), 0),
	}
	flipped := append([]byte{}, good...)
	flipped[len(flipped)-10] ^= 0x55 // inside the zstd frame: checksum mismatch
	bad["flipped"] = flipped
	for i := 0; i < len(good); i++ {
		bad[fmt.Sprintf("truncated at %d", i)] = good[:i]
	}
	for name, b := range bad {
		if _, err := decode(b); !errors.Is(err, errCorrupt) {
			t.Errorf("%s: decode err = %v, want errCorrupt", name, err)
		}
	}
	// Payload-level corruption behind a valid zstd frame.
	for name, p := range map[string][]byte{
		"empty payload":     {},
		"name idx range":    {1, 1, 'A', 1, 5, 7, 1, 0, 0},
		"asn not ascending": {0, 2, 1, 0, 0, 0},
		"first start != 0":  {0, 0, 1, 5, 0},
		"start not asc":     {0, 0, 2, 0, 0, 0, 0},
		"as idx range":      {0, 0, 1, 0, 3},
		"no entries":        {0, 0, 0},
		"huge count":        {0xff, 0xff, 0xff, 0x0f},
	} {
		if _, err := decode(wrap(t, p)); !errors.Is(err, errCorrupt) {
			t.Errorf("%s: decode err = %v, want errCorrupt", name, err)
		}
	}
	if _, err := decode(wrap(t, []byte{0, 0, 1, 0, 0})); err != nil {
		t.Errorf("minimal valid payload: %v", err)
	}
}

// wrap puts a raw payload into a table with a valid header and zstd frame.
func wrap(t *testing.T, payload []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	b.WriteString(magic)
	b.Write([]byte{1, 'v', 1, 's'})
	b.Write(zstdCompress(t, payload))
	return b.Bytes()
}

func TestCorruptLookupFailsGracefullyAndLogsOnce(t *testing.T) {
	var mu sync.Mutex
	var fails int
	d := newDB([]byte("garbage"), func(error) { mu.Lock(); fails++; mu.Unlock() })
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if got := d.lookup(netip.MustParseAddr("1.1.1.1")); got != (Info{}) {
					t.Errorf("lookup on corrupt table = %+v", got)
				}
			}
		}()
	}
	wg.Wait()
	if fails != 1 {
		t.Errorf("onFail called %d times, want 1", fails)
	}
	if d.get() != nil {
		t.Error("corrupt table decoded")
	}
}

// Numbers for the report: go test -run TestLoadStats -v ./internal/asn
func TestLoadStats(t *testing.T) {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	rss0 := vmRSS()
	start := time.Now()
	tb, err := decode(embedded)
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	t.Logf("embedded %d bytes, version %s: %d entries, %d ASes, %d names (%d bytes); load %v; heap after load +%.2f MiB; RSS %d -> %d KiB",
		len(embedded), tb.version, len(tb.starts), len(tb.asns)-1, len(tb.nameOff)-1, len(tb.names),
		took, float64(int64(after.HeapAlloc)-int64(before.HeapAlloc))/(1<<20), rss0, vmRSS())
	runtime.KeepAlive(tb)
}

// vmRSS is the resident set size in KiB on Linux, else 0.
func vmRSS() int {
	b, _ := os.ReadFile("/proc/self/status")
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) >= 2 && f[0] == "VmRSS:" {
			n, _ := strconv.Atoi(f[1])
			return n
		}
	}
	return 0
}

func BenchmarkLookup(b *testing.B) {
	std.get()
	ips := make([]netip.Addr, 1024)
	x := uint32(0x9e3779b9)
	for i := range ips {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		ips[i] = netip.AddrFrom4([4]byte{byte(x >> 24), byte(x >> 16), byte(x >> 8), byte(x)})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Lookup(ips[i&1023])
	}
}

func BenchmarkLoad(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := decode(embedded); err != nil {
			b.Fatal(err)
		}
	}
}

// Command asngen converts iptoasn.com's ip2asn-v4.tsv(.gz) into the compact
// table embedded by internal/asn. It is run by the Dockerfile's asndb target
// and is not part of the release binaries.
//
//	asngen -in ip2asn-v4.tsv.gz -out internal/asn/data/ip2asn-v4.bin -version 2026-09-26
package main

import (
	"bufio"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/asn"
)

func main() {
	in := flag.String("in", "", "ip2asn-v4.tsv or ip2asn-v4.tsv.gz (default stdin)")
	out := flag.String("out", "", "output table file")
	version := flag.String("version", time.Now().UTC().Format(time.DateOnly), "table version (build date, YYYY-MM-DD)")
	source := flag.String("source", "https://iptoasn.com/data/ip2asn-v4.tsv.gz", "data source recorded in the table")
	minRouted := flag.Int("min-routed", 100000, "fail if the input has fewer routed ranges (guards against truncated downloads)")
	flag.Parse()
	log.SetFlags(0)
	if *out == "" {
		log.Fatal("asngen: -out is required")
	}
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`).MatchString(*version) {
		log.Fatalf("asngen: -version %q is not YYYY-MM-DD", *version)
	}
	if err := run(*in, *out, *version, *source, *minRouted); err != nil {
		log.Fatalf("asngen: %v", err)
	}
}

func run(in, out, version, source string, minRouted int) error {
	var r io.Reader = os.Stdin
	if in != "" {
		f, err := os.Open(in)
		if err != nil {
			return err
		}
		defer f.Close()
		r = f
	}
	br := bufio.NewReader(r)
	if head, _ := br.Peek(2); len(head) == 2 && head[0] == 0x1f && head[1] == 0x8b {
		zr, err := gzip.NewReader(br)
		if err != nil {
			return err
		}
		defer zr.Close()
		r = zr
	} else {
		r = br
	}
	tmp, err := os.CreateTemp(filepath.Dir(out), ".asngen-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	st, err := asn.Build(tmp, r, version, source)
	if err == nil && st.Routed < minRouted {
		err = fmt.Errorf("only %d routed ranges (want >= %d); truncated input?", st.Routed, minRouted)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), out); err != nil {
		return err
	}
	fmt.Printf("asngen: %s version %s: %d lines, %d routed ranges -> %d entries, %d ASes, %d names, %d bytes\n",
		out, version, st.Lines, st.Routed, st.Entries, st.ASes, st.Names, st.Bytes)
	return nil
}

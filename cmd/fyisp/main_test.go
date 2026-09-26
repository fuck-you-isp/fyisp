package main

import (
	"slices"
	"testing"
)

func TestAllowHostFlag(t *testing.T) {
	c, err := parseFlags([]string{"--allow-host", "NAS.local, fyisp.lan:3000", "--allow-host=box.tail1234.ts.net"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"nas.local", "fyisp.lan:3000", "box.tail1234.ts.net"}
	if !slices.Equal(c.allowHosts, want) {
		t.Errorf("allowHosts = %q, want %q", c.allowHosts, want)
	}
	if _, err := parseFlags([]string{"--allow-host", "http://nas.local/"}); err == nil {
		t.Error("URL accepted as --allow-host")
	}
}

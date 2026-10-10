package main

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// adminTokenFile holds the generated admin token in the data directory, so
// the "open this link" URL stays valid across restarts. Delete it (and
// restart) to revoke every browser session.
const adminTokenFile = "admin-token"

// adminTokenOff is --admin-token=off: no token, so dashboard controls stay
// disabled on a network listener.
const adminTokenOff = "off"

// resolveAdminToken returns the admin token for the local listener at addr
// (the address actually bound) and whether it came from dir, i.e. was not
// given with --admin-token:
//   - "off": none;
//   - an explicit token: that token;
//   - a loopback listener: none (controls need no token there);
//   - otherwise the token in dir/admin-token, created on first use.
func resolveAdminToken(flagVal, addr, dir string) (tok string, stored bool, err error) {
	switch {
	case flagVal == adminTokenOff:
		return "", false, nil
	case flagVal != "":
		return flagVal, false, nil
	case isLoopbackAddr(addr):
		return "", false, nil
	}
	p := filepath.Join(dir, adminTokenFile)
	b, err := os.ReadFile(p)
	if err == nil {
		tok = strings.TrimSpace(string(b))
		if len(tok) < 32 || strings.ContainsAny(tok, " \t\r\n;,\"\\") {
			return "", false, fmt.Errorf("%s: not a valid admin token; delete the file to generate a new one", p)
		}
		if runtime.GOOS != "windows" {
			if fi, err := os.Stat(p); err == nil && fi.Mode().Perm()&0o077 != 0 {
				if err := os.Chmod(p, 0o600); err != nil {
					return "", false, err
				}
			}
		}
		return tok, true, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", false, err
	}
	if tok, err = randomHex(16); err != nil {
		return "", false, err
	}
	// Write and rename, so a crash never leaves a partial token behind. The
	// store's lock (held by the caller) keeps other instances out.
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(tok+"\n"), 0o600); err != nil {
		return "", false, err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return "", false, err
	}
	return tok, true, nil
}

// isLoopbackAddr reports whether a listen address only accepts clients on
// this machine.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// adminLink is the "open this link" URL for the dashboard at localURL. The
// token rides in the fragment, which browsers never send to a server or in
// a Referer; the page exchanges it for a session cookie and removes it.
func adminLink(localURL, tok string) string {
	return localURL + "#admin=" + tok
}

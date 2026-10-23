package main

import (
	"fmt"
	"strings"
)

// minAdminToken is the shortest --admin-token accepted: as long as a
// generated one (16 random bytes in hex), since the token guards the whole
// dashboard, through a tunnel too.
const minAdminToken = 32

// checkAdminToken refuses a --admin-token (or $FYISP_ADMIN_TOKEN) that is
// too short, or has characters a cookie or the link's fragment would mangle.
func checkAdminToken(tok string) error {
	if tok == "off" {
		return fmt.Errorf("--admin-token=off is gone: the dashboard always needs a token (leave it out for a new random one at each start)")
	}
	if len(tok) < minAdminToken {
		return fmt.Errorf("--admin-token (or FYISP_ADMIN_TOKEN): at least %d characters (e.g. openssl rand -hex 16)", minAdminToken)
	}
	if strings.Trim(tok, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~") != "" {
		return fmt.Errorf("--admin-token (or FYISP_ADMIN_TOKEN): only letters, digits and - . _ ~ (e.g. openssl rand -hex 16)")
	}
	return nil
}

// resolveAdminToken returns the admin token every dashboard route but the
// page itself needs: flagVal (--admin-token, else $FYISP_ADMIN_TOKEN,
// checked by parseFlags), or a new random one when it is empty. A generated
// token is never stored, so each start revokes every browser session; pass
// the same token to keep them, and a new one to rotate it.
func resolveAdminToken(flagVal string) (tok string, generated bool, err error) {
	if flagVal != "" {
		return flagVal, false, nil
	}
	tok, err = randomHex(minAdminToken / 2)
	return tok, err == nil, err
}

// adminLink is the "open this link" URL for the dashboard at localURL. The
// token rides in the fragment, which browsers never send to a server or in
// a Referer; the page exchanges it for a session cookie and removes it.
func adminLink(localURL, tok string) string {
	return localURL + "#admin=" + tok
}

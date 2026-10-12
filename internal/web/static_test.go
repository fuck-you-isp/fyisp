package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// The page runs under script-src 'self'; style-src 'self' (api.go) and builds
// its DOM with h() (core.js). These checks keep it that way: no HTML strings
// parsed in JS, and nothing inline in index.html that the CSP would block.

// jsBanned are ways to run strings as HTML, script or inline style, which
// the CSP would block or which bypass h(): style goes through CSSOM
// (el.style.setProperty), handlers through addEventListener.
var jsBanned = []struct {
	re   *regexp.Regexp
	what string
}{
	{regexp.MustCompile(`\b(innerHTML|outerHTML|insertAdjacentHTML|setHTMLUnsafe|createContextualFragment|DOMParser|srcdoc)\b`), "an HTML string sink"},
	{regexp.MustCompile(`\bdocument\.write(ln)?\b`), "document.write"},
	{regexp.MustCompile("setAttribute(NS)?\\(\\s*(null\\s*,\\s*)?['\"`](style|on[a-z]*)['\"`]"), "setAttribute of style or an inline handler"},
	// h(tag, { style: ... }) would set a style attribute, which the CSP blocks.
	{regexp.MustCompile(`(?m)(^|[{,])\s*['"]?style['"]?\s*:`), "a style attribute in h() props"},
}

var (
	// A start tag; '>' inside quoted attribute values doesn't end it.
	tagRe      = regexp.MustCompile(`<[a-zA-Z](?:[^>"']|"[^"]*"|'[^']*')*>`)
	inlineAttr = regexp.MustCompile(`(?i)\s(on[a-z]+|style)\s*=`)
	scriptRe   = regexp.MustCompile(`(?is)<script\b[^>]*>(.*?)</script\s*>`)
	styleTagRe = regexp.MustCompile(`(?i)<style\b`)
)

func TestStaticJSNoHTMLSinks(t *testing.T) {
	files, err := fs.Glob(staticFS, "static/*.js")
	if err != nil || len(files) == 0 {
		t.Fatalf("static/*.js: %v %v", files, err)
	}
	for _, f := range files {
		b, err := fs.ReadFile(staticFS, f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		for _, p := range jsBanned {
			for _, loc := range p.re.FindAllStringIndex(src, -1) {
				line := strings.Count(src[:loc[1]], "\n") + 1
				t.Errorf("%s:%d: %s (%q): build the DOM with h()", f, line, p.what, strings.TrimSpace(src[loc[0]:loc[1]]))
			}
		}
	}
}

func TestStaticIndexNoInline(t *testing.T) {
	b, err := fs.ReadFile(staticFS, "static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)
	for _, tag := range tagRe.FindAllString(page, -1) {
		if m := inlineAttr.FindStringSubmatch(tag); m != nil {
			t.Errorf("index.html: inline %s= in %s", m[1], tag)
		}
	}
	scripts := scriptRe.FindAllStringSubmatch(page, -1)
	if len(scripts) == 0 {
		t.Error("index.html: no <script> tags found (the check is broken)")
	}
	for _, m := range scripts {
		if strings.TrimSpace(m[1]) != "" {
			t.Errorf("index.html: inline script body in %.80q", m[0])
		}
	}
	if styleTagRe.MatchString(page) {
		t.Error("index.html: inline <style>")
	}
}

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

var (
	htmlSinkRe = regexp.MustCompile(`\b(innerHTML|outerHTML|insertAdjacentHTML)\b|\bdocument\.write(ln)?\b`)
	tagRe      = regexp.MustCompile(`<[a-zA-Z][^>]*>`)
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
		for i, line := range strings.Split(string(b), "\n") {
			if m := htmlSinkRe.FindString(line); m != "" {
				t.Errorf("%s:%d uses %s: build the DOM with h()", f, i+1, m)
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

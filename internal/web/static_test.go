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

// homeCSSRe matches the new Home's selectors (style.css, scoped under .hp).
var homeCSSRe = regexp.MustCompile(`\.hp(-[a-z0-9-]+)?\b`)

// Hover styles on Home only where a pointer can hover: on a touch screen a
// tap leaves :hover stuck on the tapped tile.
func TestStaticHomeHoverOnlyWithPointer(t *testing.T) {
	b, err := fs.ReadFile(staticFS, "static/style.css")
	if err != nil {
		t.Fatal(err)
	}
	src := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllStringFunc(string(b), func(c string) string {
		return strings.Repeat("\n", strings.Count(c, "\n")) // keep line numbers
	})
	var stack []string // the preludes of the enclosing blocks
	if strings.TrimSpace(src) == "" {
		t.Fatal("style.css is empty")
	}
	start := 0
	for i, c := range src {
		switch c {
		case '{':
			prelude := strings.Join(strings.Fields(src[start:i]), " ")
			if strings.Contains(prelude, ":hover") && homeCSSRe.MatchString(prelude) && !strings.HasPrefix(prelude, "@") {
				ok := false
				for _, p := range stack {
					if strings.HasPrefix(p, "@media") && strings.Contains(strings.ReplaceAll(p, " ", ""), "(hover:hover)") {
						ok = true
					}
				}
				if !ok {
					t.Errorf("style.css:%d: %q: Home's :hover belongs inside @media (hover: hover)", strings.Count(src[:i], "\n")+1, prelude)
				}
			}
			stack = append(stack, prelude)
			start = i + 1
		case '}':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			start = i + 1
		case ';':
			start = i + 1 // a declaration or an at-rule statement ends here
		}
	}
}

// jsStrings returns the string and template literals of a JS source with
// their line numbers, skipping comments. Template ${…} parts stay in the
// literal; Home's copy rules apply to the words either way.
func jsStrings(src string) (lits []string, lines []int) {
	line := 1
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case c == '\n':
			line++
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			line++
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return
			}
			line += strings.Count(src[i:i+2+end+2], "\n")
			i += 2 + end + 1
		case c == '\'' || c == '"' || c == '`':
			j, at := i+1, line
			for j < len(src) && src[j] != c {
				if src[j] == '\\' {
					j++
				} else if src[j] == '\n' {
					line++
				}
				j++
			}
			if j <= len(src) {
				lits = append(lits, src[i+1:min(j, len(src))])
				lines = append(lines, at)
			}
			i = j
		}
	}
	return
}

// Home's own copy follows the server's rules (home2 goldens): "internet
// provider", never "ISP"; a cause is always "Likely cause:".
func TestStaticHomeCopy(t *testing.T) {
	files, err := fs.Glob(staticFS, "static/home*.js")
	if err != nil || len(files) < 3 {
		t.Fatalf("static/home*.js: %v %v", files, err)
	}
	isp := regexp.MustCompile(`\bISP\b`)
	cause := regexp.MustCompile(`(?i)(\w*)\s*cause:`)
	for _, f := range files {
		b, err := fs.ReadFile(staticFS, f)
		if err != nil {
			t.Fatal(err)
		}
		lits, lines := jsStrings(string(b))
		if len(lits) < 10 {
			t.Fatalf("%s: only %d string literals found (the scanner is broken)", f, len(lits))
		}
		for k, s := range lits {
			if isp.MatchString(s) {
				t.Errorf("%s:%d: %q: say \"internet provider\", not ISP", f, lines[k], s)
			}
			for _, m := range cause.FindAllStringSubmatch(s, -1) {
				if !strings.EqualFold(m[1], "likely") {
					t.Errorf("%s:%d: %q: a cause is always \"Likely cause:\"", f, lines[k], s)
				}
			}
		}
	}
}

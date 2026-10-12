package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/netinfo"
	"github.com/fuck-you-isp/fyisp/internal/web/homescen"
)

// home2Now is the goldens' time: Friday 2026-10-09 10:30 am in New York
// (the UI tests' zone).
var home2Now = time.Date(2026, 10, 9, 14, 30, 0, 0, time.UTC)

const home2TZ = "America/New_York"

// home2Server is the local handler over a scenario, as fyisp would serve
// it to the scenario's viewer.
func home2Server(t *testing.T, sc *homescen.V2, now time.Time) *localHandler {
	t.Helper()
	d := Deps{Profile: func() *model.Profile { return sc.Profile }, Status: func() Status { return Status{} },
		Tiles: sc, Path: func() netinfo.Path { return sc.Path }, CustomProfile: sc.CustomProfile}
	h := Local(d, LocalOptions{Addr: sc.Viewer.Listen, CSRFToken: "t"}).(*localHandler)
	h.now = func() time.Time { return now }
	h.inContainer = func() bool { return sc.Container }
	h.own = &ownAddrs{list: func() ([]netip.Addr, error) { return sc.Viewer.Own, nil }, now: h.now}
	if sc.Speed != nil {
		h.h2speed = sc.Speed
	}
	return h
}

// home2Get asks h as the scenario's viewer.
func home2Get(t *testing.T, h http.Handler, v homescen.Viewer, query string) (int, []byte) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "http://"+v.Host+"/api/home2?"+query, nil)
	r.Host, r.RemoteAddr = v.Host, v.RemoteAddr
	for k, x := range v.Header {
		r.Header.Set(k, x)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	b, _ := io.ReadAll(w.Result().Body)
	return w.Code, b
}

// TestHome2Golden pins every new-Home scenario (internal/web/homescen,
// V2Names; the ones fyisp-webdev -home2 serves): part=now and the panels
// of zoom, m365 and the scenario's own tiles (homescen.V2Tiles), as the
// scenario's viewer gets them, in New York time. The files under
// testdata/home2 are where the copy is reviewed (and the UI tests'
// fixtures come from: test/ui/fixtures/gen.js); rewrite them with
// go test ./internal/web -run TestHome2Golden -update.
func TestHome2Golden(t *testing.T) {
	for _, name := range homescen.V2Names {
		now := home2Now
		if name == "dst" {
			now = homescen.DSTNow
		}
		sc, err := homescen.BuildV2(name, now)
		if err != nil {
			t.Fatal(err)
		}
		h := home2Server(t, sc, now)
		files := map[string][]byte{}
		code, b := home2Get(t, h, sc.Viewer, "part=now&tz="+home2TZ)
		if code != http.StatusOK {
			t.Fatalf("%s: part=now: %d %s", name, code, b)
		}
		files[name+".now.json"] = b
		var nowJSON Home2Now
		if err := json.Unmarshal(b, &nowJSON); err != nil {
			t.Fatal(err)
		}
		checkHome2Copy(t, name, &nowJSON)
		for _, id := range append([]string{"zoom", "m365"}, homescen.V2Tiles[name]...) {
			code, b := home2Get(t, h, sc.Viewer, "part=tile&id="+id+"&tz="+home2TZ)
			files[name+".tile-"+id+".json"] = b
			switch code {
			case http.StatusOK:
				var d Home2TileDetail
				if err := json.Unmarshal(b, &d); err != nil {
					t.Fatal(err)
				}
				checkHome2TileCopy(t, name+" "+id, &d)
			case http.StatusNotFound:
				if i := tileIndex(id); nowJSON.Tiles[i].State != "off" {
					t.Errorf("%s: part=tile id=%s: 404 for a tile that is %s", name, id, nowJSON.Tiles[i].State)
				}
			default:
				t.Errorf("%s: part=tile id=%s: %d %s", name, id, code, b)
			}
		}
		for f, b := range files {
			got := goldenJSON(t, b)
			path := filepath.Join("testdata", "home2", f)
			if *update {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				continue
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update)", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s differs from the golden file; run go test ./internal/web -run TestHome2Golden -update and review the diff", path)
			}
		}
	}
}

func tileIndex(id string) int {
	for i, n := range []string{"zoom", "teams", "meet", "slack", "m365", "gws", "github", "dropbox", "onedrive", "youtube", "netflix"} {
		if n == id {
			return i
		}
	}
	return -1
}

// The copy rules of the new Home (docs/HOME-API.md, v2).
var (
	// Never on Home outside a panel's figures: "ISP", ms, jitter, loss, and
	// no plan or fiber anywhere.
	banned2 = regexp.MustCompile(`(?i)\b(isps?|ms|jitter|loss|plans?|fiber|fibre)\b`)
	// US spelling.
	ukSpelling = regexp.MustCompile(`(?i)\b(colour\w*|behaviour\w*|favourite|centre|recognis\w*|organis\w*|analys(e|ed|ing)|licence|programme|travell\w*|neighbour\w*|realis\w*|apologis\w*|catalogue|minimis\w*|prioritis\w*|optimis\w*|utilis\w*|signalling|whilst)\b`)
	// No relative times in the card's text, which never changes under one key.
	relative = regexp.MustCompile(`(?i)\b(ago|just now|in a (few|moment)|recently|minutes? from now|right now for)\b`)
	allN     = regexp.MustCompile(`\ball (\d+) apps\b`)
)

// checkText applies the rules every sentence follows.
func checkText(t *testing.T, where, s string) {
	t.Helper()
	if m := ukSpelling.FindString(s); m != "" {
		t.Errorf("%s: %q: UK spelling %q", where, s, m)
	}
	if strings.Contains(s, "Cause:") || strings.Count(s, "cause:") != strings.Count(s, "Likely cause:") {
		t.Errorf("%s: %q: a cause not called likely", where, s)
	}
	if strings.Contains(s, "'") {
		t.Errorf("%s: %q: a straight apostrophe (use ’)", where, s)
	}
}

// checkHome2Copy runs part=now's words through the copy rules.
func checkHome2Copy(t *testing.T, name string, d *Home2Now) {
	t.Helper()
	texts := map[string]string{"footer": d.Footer, "path_hint": d.PathHint, "headline": d.Answer.Headline, "sub": d.Answer.Sub,
		"tab": d.Answer.Tab, "problems.summary": d.Problems.Summary}
	if d.Learning != nil {
		texts["learning"] = d.Learning.Text
	}
	if d.Answer.Day != nil {
		texts["day"] = d.Answer.Day.Text
		if !strings.Contains(d.Answer.Day.Text, d.Answer.Day.Link) {
			t.Errorf("%s: the day line %q lacks its link %q", name, d.Answer.Day.Text, d.Answer.Day.Link)
		}
	}
	for _, h := range d.Path {
		texts["path "+h.ID] = h.Label + " / " + h.Sub
	}
	for _, x := range d.Tiles {
		texts["tile "+x.ID] = strings.Join([]string{x.Word, x.Note, x.Aria, x.Reason, x.EnableHint, x.BarsSummary}, " / ")
		for _, b := range x.Bars {
			texts["bar "+x.ID] += b.Note + " / "
		}
		// The card names a router or provider cause once; tiles don't repeat it.
		if r := x.Reason; strings.Contains(r, "your internet provider") || strings.Contains(r, "your home router") {
			t.Errorf("%s: tile %s repeats the connection's cause: %q", name, x.ID, r)
		}
		if len(x.Bars) != 24 && x.State != "off" || x.State == "off" && len(x.Bars) != 0 {
			t.Errorf("%s: tile %s (%s) has %d bars", name, x.ID, x.State, len(x.Bars))
		}
	}
	if d.Speed.Latest != nil {
		texts["speed"] = d.Speed.Latest.Text + " / " + d.Speed.Latest.Note
	}
	texts["speed.empty"] = d.Speed.EmptyText
	for _, p := range d.Problems.Items {
		texts["problem "+p.ID] = strings.Join([]string{p.When, p.What, "Likely cause: " + p.Cause + ".", p.Evidence}, " / ")
		switch p.CauseKind {
		case "provider", "home", "app", "speedtest", "other":
		default:
			t.Errorf("%s: problem %s: cause_kind %q", name, p.ID, p.CauseKind)
		}
	}
	for k, s := range texts {
		checkText(t, name+" "+k, s)
		if m := banned2.FindString(s); m != "" {
			t.Errorf("%s: %s %q says %q", name, k, s, m)
		}
	}
	for _, s := range []string{d.Answer.Headline, d.Answer.Sub} {
		if m := relative.FindString(s); m != "" {
			t.Errorf("%s: the card %q has a relative time %q", name, s, m)
		}
		if m := allN.FindStringSubmatch(s); m != nil && m[1] != fmt.Sprint(d.Monitored) {
			t.Errorf("%s: %q, but %d apps are monitored", name, s, d.Monitored)
		}
	}
	if len(d.Tiles) != 11 || len(d.Path) != 3 {
		t.Errorf("%s: %d tiles and %d path nodes", name, len(d.Tiles), len(d.Path))
	}
	switch d.Answer.Tone {
	case "ok", "warn", "bad", "stale", "checking", "info":
	default:
		t.Errorf("%s: tone %q", name, d.Answer.Tone)
	}
	switch d.Answer.Icon {
	case "good", "slow", "down", "none", "info":
	default:
		t.Errorf("%s: icon %q", name, d.Answer.Icon)
	}
}

// checkHome2TileCopy runs a panel's words through the rules: its figures
// (metrics) and the chart's text alternative (a chart in ms) may say ms.
func checkHome2TileCopy(t *testing.T, name string, d *Home2TileDetail) {
	t.Helper()
	texts := map[string]string{"reason": d.Reason, "coverage": d.Coverage}
	for _, b := range d.Bands {
		texts["band"] += b.Text + " / "
	}
	for k, s := range texts {
		checkText(t, name+" "+k, s)
		if m := banned2.FindString(s); m != "" {
			t.Errorf("%s: %s %q says %q", name, k, s, m)
		}
	}
	checkText(t, name+" text", d.Text)
	if m := banned2.FindString(strings.ReplaceAll(d.Text, " ms", "")); m != "" {
		t.Errorf("%s: text %q says %q", name, d.Text, m)
	}
	if len(d.Ms) != 288 {
		t.Errorf("%s: %d chart points", name, len(d.Ms))
	}
}

// goldenJSON indents b for review: objects of a few plain values (bars,
// figures, bands) and arrays of numbers stay on one line.
func goldenJSON(t *testing.T, b []byte) []byte {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	n, err := readNode(dec)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	n.write(&out, "")
	out.WriteByte('\n')
	return out.Bytes()
}

// node is a JSON value with its keys in order.
type node struct {
	obj, arr bool
	keys     []string
	kids     []node
	raw      string // a plain value, as JSON
}

func readNode(dec *json.Decoder) (node, error) {
	tok, err := dec.Token()
	if err != nil {
		return node{}, err
	}
	switch v := tok.(type) {
	case json.Delim:
		n := node{obj: v == '{', arr: v == '['}
		for dec.More() {
			if n.obj {
				k, err := dec.Token()
				if err != nil {
					return n, err
				}
				n.keys = append(n.keys, k.(string))
			}
			kid, err := readNode(dec)
			if err != nil {
				return n, err
			}
			n.kids = append(n.kids, kid)
		}
		_, err := dec.Token()
		return n, err
	default:
		b, err := json.Marshal(v)
		return node{raw: string(b)}, err
	}
}

func (n node) plain() bool { return !n.obj && !n.arr }

// flat: written on one line.
func (n node) flat() bool {
	if n.plain() {
		return true
	}
	for _, k := range n.kids {
		if !k.plain() {
			return false
		}
	}
	return n.arr || len(n.kids) <= 4
}

func (n node) write(w *bytes.Buffer, ind string) {
	switch {
	case n.plain():
		w.WriteString(n.raw)
		return
	case n.flat():
		open, end := "{", "}"
		if n.arr {
			open, end = "[", "]"
		}
		w.WriteString(open)
		for i, k := range n.kids {
			if i > 0 {
				w.WriteString(", ")
			}
			if n.obj {
				kb, _ := json.Marshal(n.keys[i])
				w.Write(kb)
				w.WriteString(": ")
			}
			w.WriteString(k.raw)
		}
		w.WriteString(end)
		return
	}
	open, end := "{", "}"
	if n.arr {
		open, end = "[", "]"
	}
	w.WriteString(open)
	if len(n.kids) == 0 {
		w.WriteString(end)
		return
	}
	in := ind + "  "
	for i, k := range n.kids {
		if i > 0 {
			w.WriteByte(',')
		}
		w.WriteString("\n" + in)
		if n.obj {
			kb, _ := json.Marshal(n.keys[i])
			w.Write(kb)
			w.WriteString(": ")
		}
		k.write(w, in)
	}
	w.WriteString("\n" + ind + end)
}

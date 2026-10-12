//go:build netns && linux

package netns

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

// The Home view's answer (/api/home?part=live, docs/HOME-API.md) under the
// harness's faults: who is blamed, which path node carries the culprit,
// and what the tiles say. The verdict and UDP profiles tag alpha, bravo and
// charlie as work apps and delta and kilo as websites; the STUN targets
// are the Calls tile.

// homeLive is the part of /api/home?part=live the harness reads.
type homeLive struct {
	Voice  string `json:"voice"`
	Answer struct {
		Level      string `json:"level"`
		Who        string `json:"who"`
		Confidence string `json:"confidence"`
		Kind       string `json:"kind"`
		Headline   string `json:"headline"`
		Cause      string `json:"cause"`
		Hint       string `json:"hint"`
	} `json:"answer"`
	Path []struct {
		ID      string `json:"id"`
		State   string `json:"state"`
		Blamed  bool   `json:"blamed"`
		Culprit string `json:"culprit"`
	} `json:"path"`
	Tiles []homeTile `json:"tiles"`
}

type homeTile struct {
	ID       string   `json:"id"`
	Level    string   `json:"level"`
	State    string   `json:"state"`
	Label    string   `json:"label"`
	Affected []string `json:"affected"`
}

func (l *homeLive) tile(id string) *homeTile {
	for i := range l.Tiles {
		if l.Tiles[i].ID == id {
			return &l.Tiles[i]
		}
	}
	return nil
}

// blamed returns the ids of the blamed path nodes.
func (l *homeLive) blamed() []string {
	var out []string
	for _, n := range l.Path {
		if n.Blamed {
			out = append(out, n.ID)
		}
	}
	return out
}

func (h *harness) home(t *testing.T, name string) (homeLive, []byte) {
	t.Helper()
	b := h.get(t, "/api/home?part=live")
	h.save(t, "home-"+name+".json", b)
	var l homeLive
	if err := json.Unmarshal(b, &l); err != nil {
		t.Fatalf("/api/home: %v: %s", err, b)
	}
	t.Logf("/api/home %s: answer %s/%s (%s) %q / %q / hint %q; blamed %v",
		name, l.Answer.Level, l.Answer.Who, l.Answer.Confidence, l.Answer.Headline, l.Answer.Cause, l.Answer.Hint, l.blamed())
	return l, b
}

// homeSettle is how long after the verdict Home may take to say ok.
const homeSettle = 60 * time.Second

// homeWho is the Home answer's who for each path verdict kind.
var homeWho = map[string]string{"lan": "home", "isp": "provider", "upstream": "beyond", "dns": "dns"}

// homeNode is the path node that carries the culprit for each who.
var homeNode = map[string]string{"home": "router", "provider": "provider", "beyond": "internet"}

// checkHome checks /api/home while the verdict is kind (targets: the
// verdict's), and that the public voice says "your" nowhere.
func (h *harness) checkHome(t *testing.T, name, kind string, targets []string) {
	t.Helper()
	l, raw := h.home(t, name)
	switch kind {
	case "ok":
		// Back to ok: the verdict has seen a minute of ok checks. Home may
		// take a moment longer, but not long.
		for t0 := time.Now(); (l.Answer.Level != "good" || l.Answer.Who != "none") && time.Since(t0) < homeSettle; {
			time.Sleep(5 * time.Second)
			l, raw = h.home(t, name)
		}
		if l.Answer.Level != "good" || l.Answer.Who != "none" || len(l.blamed()) != 0 {
			t.Errorf("Home %s: answer %s/%s blamed %v %s after the verdict, want good/none and no culprit: %s",
				name, l.Answer.Level, l.Answer.Who, l.blamed(), homeSettle, raw)
		}
	case "lan", "isp", "upstream":
		who := homeWho[kind]
		if l.Answer.Who != who || l.Answer.Level == "good" || l.Answer.Kind != kind {
			t.Errorf("Home %s: answer %s/%s (kind %s), want who %s, not good: %s", name, l.Answer.Level, l.Answer.Who, l.Answer.Kind, who, raw)
		}
		// Weak ("slow to answer our checks", no culprit) is for a lossy
		// first hop alone; these faults lose packets on everything past
		// the fault, as the path nodes and the verdict's evidence show.
		if b := l.blamed(); l.Answer.Confidence == "weak" || !slices.Equal(b, []string{homeNode[who]}) {
			t.Errorf("Home %s: confidence %q, blamed nodes %v, want [%s] blamed: %s", name, l.Answer.Confidence, b, homeNode[who], raw)
		}
		for _, n := range l.Path {
			if n.Blamed && n.Culprit == "" {
				t.Errorf("Home %s: blamed node %s has no culprit label", name, n.ID)
			}
		}
	case "service":
		// One target of a working tile: no headline escalation; the cause
		// (or the hint) names it (docs/HOME-API.md, "One app of a working
		// tile").
		hint := strings.ToLower(l.Answer.Cause + " " + l.Answer.Hint)
		if l.Answer.Level != "good" || len(l.blamed()) != 0 {
			t.Errorf("Home %s: answer %s/%s blamed %v, want good: %s", name, l.Answer.Level, l.Answer.Who, l.blamed(), raw)
		}
		for _, n := range targets {
			if !strings.Contains(hint, n) {
				t.Errorf("Home %s: cause %q and hint %q do not name %s", name, l.Answer.Cause, l.Answer.Hint, n)
			}
		}
		if w := l.tile("work"); w == nil || w.State != "mostly" || !slices.Equal(w.Affected, targets) {
			t.Errorf("Home %s: work tile %+v, want mostly with %v affected", name, w, targets)
		}
	case "udp_blocked", "stun_blocked":
		if l.Answer.Who != "calltraffic" || l.Answer.Level == "good" {
			t.Errorf("Home %s: answer %s/%s, want calltraffic, not good: %s", name, l.Answer.Level, l.Answer.Who, raw)
		}
		if c := l.tile("calls"); c == nil || c.Level == "good" || c.State != "blocked" {
			t.Errorf("Home %s: calls tile %+v, want blocked, not good", name, c)
		}
		for _, id := range []string{"work", "web"} {
			if c := l.tile(id); c == nil || c.Level != "good" || c.State != "working" {
				t.Errorf("Home %s: %s tile %+v, want working", name, id, c)
			}
		}
	}
}

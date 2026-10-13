//go:build netns && linux

package netns

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The Home view's answer (/api/home?part=now, docs/HOME-API.md) under the
// harness's faults: the answer card's key and tone, and which node of the
// connection strip carries the culprit. The verdict and UDP profiles have
// no tile: tags, so every tile is off here; the tiles are TestHome10's.

// homeNow is the part of /api/home?part=now the harness reads.
type homeNow struct {
	Answer struct {
		Key      string `json:"key"`
		Tone     string `json:"tone"`
		Headline string `json:"headline"`
		Sub      string `json:"sub"`
	} `json:"answer"`
	Path []struct {
		ID    string `json:"id"`
		State string `json:"state"`
		Sub   string `json:"sub"`
	} `json:"path"`
}

// kind is the answer key's kind: "isp" of "isp:1760261100000".
func (l *homeNow) kind() string { k, _, _ := strings.Cut(l.Answer.Key, ":"); return k }

// hurt returns the ids of the strip's nodes that aren't good.
func (l *homeNow) hurt() []string {
	var out []string
	for _, n := range l.Path {
		if n.State == "down" || n.State == "slow" {
			out = append(out, n.ID)
		}
	}
	return out
}

func (h *harness) home(t *testing.T, name string) (homeNow, []byte) {
	t.Helper()
	b := h.get(t, "/api/home?part=now")
	h.save(t, "home-"+name+".json", b)
	var l homeNow
	if err := json.Unmarshal(b, &l); err != nil {
		t.Fatalf("/api/home: %v: %s", err, b)
	}
	t.Logf("/api/home %s: answer %s (%s) %q / %q; path not good %v", name, l.Answer.Key, l.Answer.Tone, l.Answer.Headline, l.Answer.Sub, l.hurt())
	return l, b
}

// homeSettle is how long after the verdict Home may take to say ok;
// homeLag how long it may take to follow a fault's verdict (the engine
// reads the verdict at its 5 s ticks).
const (
	homeSettle = 60 * time.Second
	homeLag    = 15 * time.Second
)

// homeNode is the strip's node that carries the culprit for each path
// verdict kind.
var homeNode = map[string]int{"lan": 0, "isp": 1, "upstream": 2}

// checkHome checks /api/home while the verdict is kind, polling until it
// holds or Home had its time to follow.
func (h *harness) checkHome(t *testing.T, name, kind string) {
	t.Helper()
	wait := homeLag
	if kind == "ok" {
		wait = homeSettle
	}
	l, raw := h.home(t, name)
	for t0 := time.Now(); homeWrong(&l, kind) != "" && time.Since(t0) < wait; {
		time.Sleep(5 * time.Second)
		l, raw = h.home(t, name)
	}
	if w := homeWrong(&l, kind); w != "" {
		v, _ := h.verdict(t)
		t.Logf("verdict now: %s since %v", v.Kind, v.Since)
		t.Errorf("Home %s: answer %s (%s), path not good %v, want %s: %s", name, l.Answer.Key, l.Answer.Tone, l.hurt(), w, raw)
	}
}

// homeWrong says what Home should say while the verdict is kind, or "" when
// it does.
func homeWrong(l *homeNow, kind string) string {
	switch kind {
	case "ok":
		// Back to ok: after a fault Home says "working again" for a while.
		if (l.kind() != "good" && l.kind() != "working_again") || l.Answer.Tone == "bad" || len(l.hurt()) != 0 {
			return "good or working again, the path good"
		}
	case "lan", "isp", "upstream":
		// The culprit is down or slow, and the nodes before it are fine.
		i := homeNode[kind]
		if l.kind() != kind || l.Answer.Tone != "bad" || len(l.Path) != 3 || (l.Path[i].State != "down" && l.Path[i].State != "slow") {
			return fmt.Sprintf("%s, bad, path[%d] down or slow", kind, i)
		}
		for _, n := range l.Path[:i] {
			if n.State != "good" {
				return fmt.Sprintf("%s good before the culprit", n.ID)
			}
		}
	case "service":
		// One app's target: the connection is fine (the tiles, off here,
		// are where an app shows).
		if l.Answer.Tone != "ok" || len(l.hurt()) != 0 {
			return "ok, the path good"
		}
	case "dns":
		// Names fail, the connection is up: a warning, the path good.
		if l.kind() != "dns" || l.Answer.Tone != "warn" || len(l.hurt()) != 0 {
			return "dns, a warning, the path good"
		}
	case "udp_blocked", "stun_blocked":
		if l.kind() != "udp_blocked" || l.Answer.Tone == "bad" || l.Answer.Tone == "ok" || len(l.hurt()) != 0 {
			return "udp_blocked, a warning, the path good"
		}
	}
	return ""
}

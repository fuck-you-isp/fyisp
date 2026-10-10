package web

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/web/homescen"
)

var update = flag.Bool("update", false, "rewrite the golden files of TestHomeGolden")

// TestHomeGolden pins every Home scenario (internal/web/homescen, the same
// ones fyisp-webdev -home serves) in both voices: the live answer, today,
// the week, and "what happened 5 minutes ago". The files under
// testdata/home are where the copy is reviewed; rewrite them with
// go test ./internal/web -run TestHomeGolden -update.
func TestHomeGolden(t *testing.T) {
	for _, name := range homescen.Names {
		sc, err := homescen.Build(name, homeNow, homeNow.Location())
		if err != nil {
			t.Fatal(err)
		}
		for _, voice := range []string{voiceYou, voicePublic} {
			d := Deps{Profile: func() *model.Profile { return sc.Profile }, Store: sc.Store, Verdict: sc.Verdict,
				Status: func() Status { return Status{} }}
			s := newServer(d, voice == voicePublic, "")
			s.now = func() time.Time { return homeNow }
			ctx := context.Background()
			live, err := s.homeLive(ctx, voice, homeNow)
			if err != nil {
				t.Fatal(err)
			}
			hp := homeParams{part: "history", voice: voice, tz: 120}
			hp.span = "d"
			today, err := s.homeHistory(ctx, hp, homeNow)
			if err != nil {
				t.Fatal(err)
			}
			hp.span = "w"
			week, err := s.homeHistory(ctx, hp, homeNow)
			if err != nil {
				t.Fatal(err)
			}
			at, err := s.homeAt(ctx, homeNow.Add(-5*time.Minute), voice, homeNow)
			if err != nil {
				t.Fatal(err)
			}
			got, err := json.MarshalIndent(map[string]any{"live": live, "history_today": today, "history_week": week, "at_5_minutes_ago": at}, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			checkGoldenCopy(t, name+"."+voice, voice, got)
			path := filepath.Join("testdata", "home", name+"."+voice+".json")
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
				t.Errorf("%s differs from the golden file; run go test ./internal/web -run TestHomeGolden -update and review the diff", path)
			}
		}
	}
}

// copyKeys are the JSON fields that hold Home's words.
var copyKeys = map[string]bool{"headline": true, "cause": true, "hint": true, "fact": true, "announce": true, "try": true,
	"label": true, "note": true, "words": true, "line": true, "tooltip": true, "summary": true, "where": true, "title": true}

// checkGoldenCopy runs the copy of a golden response through the
// banned-word rules of TestHomeCopyWords.
func checkGoldenCopy(t *testing.T, name, voice string, b []byte) {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	your := regexp.MustCompile(`\b[Yy]our\b`)
	var walk func(key string, v any)
	walk = func(key string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, y := range x {
				walk(k, y)
			}
		case []any:
			for _, y := range x {
				walk(key, y)
			}
		case string:
			if !copyKeys[key] {
				return
			}
			if voice == voicePublic && your.MatchString(x) {
				t.Errorf("%s: public %s says your: %q", name, key, x)
			}
			if key == "try" {
				x = strings.ReplaceAll(x, "DNS", "")
			}
			if m := bannedOnHome.FindString(strings.ReplaceAll(x, "(DNS)", "")); m != "" {
				t.Errorf("%s: %s %q says %q", name, key, x, m)
			}
		}
	}
	walk("", v)
	// The headline never repeats the cause.
	var r struct {
		Live struct{ Answer HomeAnswer } `json:"live"`
		At   HomeAt                      `json:"at_5_minutes_ago"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	// One minute, one figure.
	if a, b := r.Live.Answer, r.At; a.FactAt != 0 && a.FactAt == b.FactAt && a.Fact != b.Fact {
		t.Errorf("%s: the minute %d reads %q live and %q in part=at", name, a.FactAt, a.Fact, b.Fact)
	}
	for _, x := range [][2]string{{r.Live.Answer.Headline, r.Live.Answer.Cause}, {r.At.Headline, r.At.Cause}} {
		if rep := repeats(x[0], x[1]); rep != "" {
			t.Errorf("%s: headline %q and cause %q both say %q", name, x[0], x[1], rep)
		}
	}
}

// TestHomePublicReportGolden pins the public report (the share link's
// week, through the public route, voice forced to public) for two
// scenarios, and checks its redaction: no hosts, no addresses.
func TestHomePublicReportGolden(t *testing.T) {
	hostish := regexp.MustCompile(`[a-z0-9-]+\.(com|net|org|goog|us|gg|io)\b|\b\d{1,3}(\.\d{1,3}){3}\b|evidence|region`)
	for _, name := range []string{"isp", "week-with-gaps"} {
		sc, err := homescen.Build(name, homeNow, homeNow.Location())
		if err != nil {
			t.Fatal(err)
		}
		d := Deps{Profile: func() *model.Profile { return sc.Profile }, Store: sc.Store, Verdict: sc.Verdict,
			Status: func() Status { return Status{} }}
		h := Public(d, testSecret).(*publicHandler)
		h.now = func() time.Time { return homeNow }
		w := do(h, "GET", "/s/"+testSecret+"/api/home?part=history&span=w&tz=120&voice=you", nil)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
		var v any
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		got, _ := json.MarshalIndent(v, "", "  ")
		got = append(got, '\n')
		if m := hostish.Find(got); m != nil {
			t.Errorf("%s: the public report says %q", name, m)
		}
		var hh HomeHistory
		_ = json.Unmarshal(got, &hh)
		if hh.Voice != voicePublic || len(hh.Days) != 7 || hh.Sides.Home+hh.Sides.Provider+hh.Sides.Other != hh.Counts.Problems {
			t.Errorf("%s: voice %s days %d sides %+v counts %+v", name, hh.Voice, len(hh.Days), hh.Sides, hh.Counts)
		}
		checkGoldenCopy(t, "report."+name, voicePublic, []byte(`{"history_week":`+string(got)+`}`))
		path := filepath.Join("testdata", "home", "report."+name+".json")
		if *update {
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
			t.Errorf("%s differs from the golden file; run go test ./internal/web -run TestHomePublicReportGolden -update and review the diff", path)
		}
	}
}

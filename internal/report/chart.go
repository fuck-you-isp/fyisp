package report

import (
	"html"
	"html/template"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// Chart geometry (SVG user units; the SVG scales to the page width).
const (
	cW, cH       = 760, 244
	px0, px1     = 50, 750 // plot x
	py0, py1     = 10, 184 // plot y
	sy0, sy1     = 192, 206
	xLabelY      = 224
	gridColor    = "#e1e0d9"
	axisColor    = "#c3c2b7"
	mutedColor   = "#6f6e69"
	notMeasColor = "#d9d8d2"
)

type chartLegend struct {
	Swatch template.HTML
	Label  string
}

type chartView struct {
	SVG     template.HTML
	Series  []chartLegend
	Reasons []chartLegend
	Kinds   []chartLegend
	Clipped string // "values above N ms are drawn at the top"
	Omitted int    // targets not drawn
	Step    string
	Band    bool
	NotMeas bool
}

func f1(v float64) string { return strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64) }

func lineSwatch(color string) template.HTML {
	return template.HTML(`<svg class="sw" viewBox="0 0 18 10" aria-hidden="true"><line x1="1" y1="5" x2="17" y2="5" stroke="` +
		html.EscapeString(color) + `" stroke-width="2.5" stroke-linecap="round"/></svg>`)
}

func boxSwatch(color string, opacity float64) template.HTML {
	return template.HTML(`<svg class="sw" viewBox="0 0 12 10" aria-hidden="true"><rect x="0.5" y="0.5" width="11" height="9" rx="2" fill="` +
		html.EscapeString(color) + `" fill-opacity="` + f1(opacity) + `"/></svg>`)
}

// niceStep returns a 1/2/5 × 10^k step giving about n intervals up to max.
func niceStep(max float64, n int) float64 {
	raw := max / float64(n)
	p := math.Pow(10, math.Floor(math.Log10(raw)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if m*p >= raw {
			return m * p
		}
	}
	return 10 * p
}

func percentile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sort.Float64s(xs)
	i := int(math.Ceil(q*float64(len(xs)))) - 1
	return xs[max(0, min(i, len(xs)-1))]
}

var tickSteps = []time.Duration{time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute, 30 * time.Minute,
	time.Hour, 2 * time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour, 24 * time.Hour, 48 * time.Hour, 7 * 24 * time.Hour}

// chart draws one group: a mean line per charted target (with a min–max
// band when there are at most 3), incidents shaded behind, and a loss strip
// under the plot coloured by the dominant failure reason.
func (r *run) chart(g *group) chartView {
	res := g.panel
	var cv chartView
	span := r.to.Sub(r.from).Seconds()
	x := func(t time.Time) float64 {
		return px0 + (px1-px0)*math.Max(0, math.Min(1, t.Sub(r.from).Seconds()/span))
	}
	var drawn []*seriesStat
	for _, s := range g.series {
		if s.chart && s.cols != nil {
			drawn = append(drawn, s)
		} else if s.n+s.lost > 0 {
			cv.Omitted++
		}
	}
	cv.Band = len(drawn) <= 3
	cv.Step = fmtDur(res.Step)

	// Y scale: robust max (98th percentile), so one spike does not flatten
	// the chart; clipped values are drawn at the top.
	var vals []float64
	trueMax := 0.0
	for _, s := range drawn {
		for i := range s.cols.N {
			if s.cols.N[i] == 0 || !r.inRange(res, i) {
				continue
			}
			v := float64(s.cols.Mean[i])
			if cv.Band {
				v = float64(s.cols.Max[i])
			}
			if !math.IsNaN(v) {
				vals = append(vals, v)
				trueMax = math.Max(trueMax, v)
			}
		}
	}
	yMax := percentile(vals, 0.98) * 1.15
	if yMax <= 0 {
		yMax = 10
	}
	step := niceStep(yMax, 4)
	yMax = math.Ceil(yMax/step) * step
	if trueMax > yMax {
		cv.Clipped = "Values above " + fmtMs(yMax) + " are drawn at the top edge (highest: " + fmtMs(trueMax) + ")."
	}
	y := func(v float64) float64 {
		v = math.Max(0, math.Min(v, yMax))
		return py1 - (py1-py0)*v/yMax
	}

	var b strings.Builder
	b.Grow(16 << 10)
	b.WriteString(`<svg class="chart" viewBox="0 0 ` + strconv.Itoa(cW) + ` ` + strconv.Itoa(cH) + `" role="img" aria-label="` +
		html.EscapeString("Latency and loss for "+g.g.Title) + `">`)

	// Incidents behind everything.
	kindsSeen := map[model.VerdictKind]bool{}
	for _, in := range r.incidents {
		x0, x1 := x(in.start), x(in.end)
		if x1-x0 < 1.5 {
			x1 = x0 + 1.5
		}
		c := html.EscapeString(kindOf(in.Kind).Color)
		b.WriteString(`<rect x="` + f1(x0) + `" y="` + strconv.Itoa(py0) + `" width="` + f1(x1-x0) + `" height="` + strconv.Itoa(py1-py0) +
			`" fill="` + c + `" fill-opacity="0.12"/><rect x="` + f1(x0) + `" y="` + strconv.Itoa(py0) + `" width="` + f1(x1-x0) + `" height="3" fill="` + c + `"/>`)
		kindsSeen[in.Kind] = true
	}
	for _, k := range kindOrder {
		if kindsSeen[k] {
			cv.Kinds = append(cv.Kinds, chartLegend{boxSwatch(kindOf(k).Color, 0.5), kindOf(k).Label})
			delete(kindsSeen, k)
		}
	}
	var rest []model.VerdictKind
	for k := range kindsSeen {
		rest = append(rest, k)
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i] < rest[j] })
	for _, k := range rest {
		cv.Kinds = append(cv.Kinds, chartLegend{boxSwatch(kindOf(k).Color, 0.5), kindOf(k).Label})
	}

	// Y grid and labels.
	for v := 0.0; v <= yMax+step/2; v += step {
		yy := f1(y(v))
		b.WriteString(`<line x1="` + strconv.Itoa(px0) + `" x2="` + strconv.Itoa(px1) + `" y1="` + yy + `" y2="` + yy + `" stroke="` + gridColor + `" stroke-width="1"/>`)
		label := strconv.FormatFloat(v, 'f', -1, 64)
		if v == yMax {
			label += " ms"
		}
		b.WriteString(`<text x="` + strconv.Itoa(px0-5) + `" y="` + f1(y(v)+3.5) + `" text-anchor="end" fill="` + mutedColor + `">` + label + `</text>`)
	}
	// X ticks.
	var tick time.Duration
	for _, s := range tickSteps {
		tick = s
		if r.to.Sub(r.from)/s <= 8 {
			break
		}
	}
	for t := r.from.Truncate(tick); !t.After(r.to); t = t.Add(tick) {
		if t.Before(r.from) {
			continue
		}
		xx := f1(x(t))
		b.WriteString(`<line x1="` + xx + `" x2="` + xx + `" y1="` + strconv.Itoa(py0) + `" y2="` + strconv.Itoa(sy1+3) + `" stroke="` + gridColor + `" stroke-width="1"/>`)
		var label string
		switch {
		case tick >= 24*time.Hour:
			label = t.Format("Mon 2 Jan")
		case t.Hour() == 0 && t.Minute() == 0 && r.to.Sub(r.from) > 12*time.Hour:
			label = t.Format("2 Jan")
		default:
			label = t.Format("15:04")
		}
		b.WriteString(`<text x="` + xx + `" y="` + strconv.Itoa(xLabelY) + `" text-anchor="middle" fill="` + mutedColor + `">` + label + `</text>`)
	}
	b.WriteString(`<text x="` + strconv.Itoa(px1) + `" y="` + strconv.Itoa(xLabelY+14) + `" text-anchor="end" fill="` + mutedColor + `">UTC</text>`)
	b.WriteString(`<line x1="` + strconv.Itoa(px0) + `" x2="` + strconv.Itoa(px1) + `" y1="` + strconv.Itoa(py1) + `" y2="` + strconv.Itoa(py1) + `" stroke="` + axisColor + `" stroke-width="1"/>`)

	bx := func(i int) (float64, float64) {
		bs := res.Start.Add(time.Duration(i) * res.Step)
		return x(bs), x(bs.Add(res.Step))
	}
	mid := func(i int) float64 { a, c := bx(i); return (a + c) / 2 }

	// Bands, then lines (first series on top).
	if cv.Band {
		for _, s := range drawn {
			c := s.cols
			var seg []int
			flush := func() {
				if len(seg) == 0 {
					return
				}
				b.WriteString(`<path d="`)
				for j, i := range seg {
					if j == 0 {
						b.WriteString("M")
					} else {
						b.WriteString("L")
					}
					b.WriteString(f1(mid(i)) + " " + f1(y(float64(c.Max[i]))))
				}
				for j := len(seg) - 1; j >= 0; j-- {
					i := seg[j]
					b.WriteString("L" + f1(mid(i)) + " " + f1(y(float64(c.Min[i]))))
				}
				b.WriteString(`Z" fill="` + s.color + `" fill-opacity="0.16" stroke="none"/>`)
				seg = seg[:0]
			}
			for i := range c.N {
				if c.N[i] == 0 || isNaN32(c.Min[i]) || !r.inRange(res, i) {
					flush()
					continue
				}
				seg = append(seg, i)
			}
			flush()
		}
	}
	for j := len(drawn) - 1; j >= 0; j-- {
		s := drawn[j]
		c := s.cols
		var d strings.Builder
		pen := false
		for i := range c.N {
			if c.N[i] == 0 || isNaN32(c.Mean[i]) || !r.inRange(res, i) {
				if pen {
					d.WriteString("h0.01") // a lone point shows as a dot
				}
				pen = false
				continue
			}
			if pen {
				d.WriteString("L")
			} else {
				d.WriteString("M")
			}
			d.WriteString(f1(mid(i)) + " " + f1(y(float64(c.Mean[i]))))
			pen = true
		}
		if pen {
			d.WriteString("h0.01")
		}
		if d.Len() > 0 {
			b.WriteString(`<path d="` + d.String() + `" fill="none" stroke="` + s.color + `" stroke-width="1.6" stroke-linejoin="round" stroke-linecap="round"/>`)
		}
	}
	for _, s := range drawn {
		label := s.target.Name + " (" + kindLabel(s.key.Kind) + ")"
		cv.Series = append(cv.Series, chartLegend{lineSwatch(s.color), label})
	}

	// Loss strip over every target of the group.
	b.WriteString(`<text x="` + strconv.Itoa(px0-5) + `" y="` + strconv.Itoa(sy1-3) + `" text-anchor="end" fill="` + mutedColor + `">loss</text>`)
	b.WriteString(`<rect x="` + strconv.Itoa(px0) + `" y="` + strconv.Itoa(sy0) + `" width="` + strconv.Itoa(px1-px0) + `" height="` + strconv.Itoa(sy1-sy0) + `" fill="#f4f3ef"/>`)
	nb := 0
	for _, s := range g.series {
		if s.cols != nil {
			nb = len(s.cols.N)
			break
		}
	}
	reasonsSeen := map[model.Reason]bool{}
	for i := 0; i < nb; i++ {
		if !r.inRange(res, i) {
			continue
		}
		var n, lost uint64
		by := map[model.Reason]uint64{}
		for _, s := range g.series {
			if s.cols == nil {
				continue
			}
			n += uint64(s.cols.N[i])
			lost += uint64(s.cols.Lost[i])
			for rr, cnt := range s.cols.LostBy {
				by[rr] += uint64(cnt[i])
			}
		}
		x0, x1 := bx(i)
		if x1-x0 < 0.6 {
			x1 = x0 + 0.6
		}
		if n+lost == 0 {
			b.WriteString(`<rect x="` + f1(x0) + `" y="` + strconv.Itoa(sy0) + `" width="` + f1(x1-x0) + `" height="` + strconv.Itoa(sy1-sy0) + `" fill="` + notMeasColor + `"/>`)
			cv.NotMeas = true
			continue
		}
		if lost == 0 {
			continue
		}
		dom, dc := model.ReasonOther, uint64(0)
		for rr, cnt := range by {
			if cnt > dc || (cnt == dc && rr < dom) {
				dom, dc = rr, cnt
			}
		}
		reasonsSeen[dom] = true
		loss := float64(lost) / float64(n+lost)
		op := 0.3 + 0.7*math.Min(1, loss*2)
		b.WriteString(`<rect x="` + f1(x0) + `" y="` + strconv.Itoa(sy0) + `" width="` + f1(x1-x0) + `" height="` + strconv.Itoa(sy1-sy0) +
			`" fill="` + reasonColor(dom) + `" fill-opacity="` + f1(op) + `"/>`)
	}
	var rs []model.Reason
	for rr := range reasonsSeen {
		rs = append(rs, rr)
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i] < rs[j] })
	for _, rr := range rs {
		cv.Reasons = append(cv.Reasons, chartLegend{boxSwatch(reasonColor(rr), 1), "lost: " + rr.String()})
	}
	if cv.NotMeas {
		cv.Reasons = append(cv.Reasons, chartLegend{boxSwatch(notMeasColor, 1), "not measured"})
	}
	b.WriteString(`</svg>`)
	cv.SVG = template.HTML(b.String())
	return cv
}

func kindLabel(k model.ProbeKind) string {
	switch k {
	case model.KindHTTPS:
		return "HTTPS"
	case model.KindTCP:
		return "TCP"
	case model.KindICMP:
		return "ICMP"
	}
	return k.String()
}

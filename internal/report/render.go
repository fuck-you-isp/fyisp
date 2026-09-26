package report

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"html/template"
	"math"
	"sort"
	"strings"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/profile"
)

// CSP is the Content-Security-Policy a report needs (and carries in a
// <meta> tag): no scripts, no external resources, only its own stylesheet.
// Serve reports with it, plus the usual frame-ancestors 'none'.
func CSP() string { return csp }

var csp = "default-src 'none'; style-src 'sha256-" + func() string {
	h := sha256.Sum256([]byte(css))
	return base64.StdEncoding.EncodeToString(h[:])
}() + "'; img-src data:; base-uri 'none'; form-action 'none'"

type doc struct {
	CSS      template.CSS
	CSP      string
	Title    string
	From, To string
	Span     string
	Now      string
	Version  string
	ISP      string
	Redacted bool
	Method   []string

	HasData  bool
	Headline string
	Good     bool
	Findings []string
	Totals   []totalRow

	Incidents  []incRow
	IncTotal   int
	IncOmitted int

	Base     bool
	Overview []ovRow
	Charts   []groupChart
	StatStep string

	HasTrace bool
	Traces   []*traceView

	HasNotes bool
	Notes    []noteRow
}

type totalRow struct {
	Swatch           template.HTML
	Label            string
	Count, Dur, Days string
	PeakLoss         string
}

type incRow struct {
	Start, End, Dur string
	Swatch          template.HTML
	Label           string
	Summary         string
	Peak            string
	Targets         string
}

type ovRow struct {
	Title        string
	Targets      int
	Typical, P95 string
	Loss, Ratio  string
	Hot, Charted bool
	NoData       bool
}

type groupChart struct {
	Title string
	Chart chartView
	Rows  []statRow
	Base  bool
}

type statRow struct {
	Swatch                  template.HTML
	Name, Kind              string
	Mean, Median, P95, Loss string
	Ratio, Normal, Reasons  string
	Hot, LossHot, NoData    bool
}

type noteRow struct {
	When, Text string
	Private    bool
}

func (r *run) render() ([]byte, error) {
	d := doc{
		CSS: template.CSS(css), CSP: csp,
		Title: r.o.Title, From: fmtLongTime(r.from), To: fmtLongTime(r.to), Span: fmtDur(r.to.Sub(r.from)),
		Now: fmtLongTime(r.now), Redacted: r.o.Redact, HasData: r.hasData,
		HasTrace: r.b.d.Trace != nil, Base: r.baselines, HasNotes: r.b.d.Annotations != nil,
	}
	if d.Title == "" {
		d.Title = "Internet connection evidence report"
	}
	if !r.o.Redact {
		d.Version = r.b.d.Version
	}
	if r.ispASN != 0 {
		d.ISP = fmt.Sprintf("AS%d", r.ispASN)
		if r.ispOwner != "" {
			d.ISP += " " + r.ispOwner
		}
	}
	d.Method = r.method()
	totals := r.kindTotals()
	d.Headline, d.Findings = r.summary(totals)
	d.Good = len(r.incidents) == 0
	for _, t := range totals {
		d.Totals = append(d.Totals, totalRow{Swatch: boxSwatch(t.Info.Color, 1), Label: t.Info.Label,
			Count: fmt.Sprint(t.Count), Dur: fmtDur(t.Dur), Days: fmt.Sprint(t.Days), PeakLoss: fmtPct(t.PeakLoss)})
	}

	// Outage log: at most MaxIncidentRows (the longest), in time order.
	incs := r.incidents
	d.IncTotal = len(incs)
	if len(incs) > MaxIncidentRows {
		incs = append([]incident(nil), incs...)
		sort.SliceStable(incs, func(i, j int) bool { return incs[i].dur() > incs[j].dur() })
		incs = incs[:MaxIncidentRows]
		sort.SliceStable(incs, func(i, j int) bool { return incs[i].Start.Before(incs[j].Start) })
		d.IncOmitted = len(r.incidents) - MaxIncidentRows
	}
	for _, in := range incs {
		ki := kindOf(in.Kind)
		row := incRow{Start: fmtTableTime(in.Start), Dur: fmtDur(in.dur()), Swatch: boxSwatch(ki.Color, 1), Label: ki.Label,
			Summary: in.Summary, Peak: fmtPct(in.PeakLoss)}
		if in.ongoing {
			row.End = "ongoing"
		} else {
			row.End = fmtTableTime(in.End)
		}
		switch n := len(in.Targets); {
		case n == 0:
			row.Targets = "whole connection"
		case n <= 8:
			row.Targets = strings.Join(in.Targets, ", ")
		default:
			row.Targets = strings.Join(in.Targets[:8], ", ") + fmt.Sprintf(" and %d more", n-8)
		}
		d.Incidents = append(d.Incidents, row)
	}

	// Groups.
	for _, g := range r.groups {
		ov := ovRow{Title: g.g.Title, Targets: len(g.series), Charted: g.charted}
		if g.n+g.lost == 0 {
			ov.NoData = true
		} else {
			ov.Typical = fmtMs(g.typical)
			var p95s []float64
			for _, s := range g.series {
				if !math.IsNaN(s.p95) {
					p95s = append(p95s, s.p95)
				}
			}
			ov.P95 = fmtMs(median(p95s))
			ov.Loss = fmtPct(g.loss())
			ov.Ratio = fmtRatio(g.ratio)
			ov.Hot = g.ratio >= 1.5 || g.loss() >= 0.01
		}
		d.Overview = append(d.Overview, ov)
		if !g.charted {
			continue
		}
		if d.StatStep == "" && g.panel != nil {
			d.StatStep = fmtDur(g.panel.Step)
		}
		gc := groupChart{Title: g.g.Title, Chart: r.chart(g), Base: r.baselines}
		if g.g.ID == profile.PathGroup {
			gc.Title += " (your router, your ISP's first hop, anycast resolvers)"
		}
		for _, s := range g.series {
			row := statRow{Name: s.target.Name, Kind: kindLabel(s.key.Kind)}
			if s.chart {
				row.Swatch = lineSwatch(s.color)
			}
			if s.n+s.lost == 0 {
				row.NoData = true
				gc.Rows = append(gc.Rows, row)
				continue
			}
			row.Mean, row.Median, row.P95 = fmtMs(s.mean), fmtMs(s.median), fmtMs(s.p95)
			row.Loss, row.LossHot = fmtPct(s.loss()), s.loss() >= 0.01
			row.Reasons = reasonsText(s.lostBy)
			if s.base != nil {
				row.Normal = fmtMs(s.base.MedianMs)
				row.Ratio = fmtRatio(s.ratio)
				row.Hot = s.ratio >= 1.5
			}
			gc.Rows = append(gc.Rows, row)
		}
		d.Charts = append(d.Charts, gc)
	}
	d.Traces = r.traces

	for _, a := range r.notes {
		when := fmtTableTime(a.At)
		if !a.End.IsZero() {
			when += " – " + fmtTableTime(a.End) + " (" + fmtDur(a.End.Sub(a.At)) + ")"
		}
		d.Notes = append(d.Notes, noteRow{When: when, Text: a.Text, Private: !a.Public})
	}

	var buf bytes.Buffer
	buf.Grow(256 << 10)
	if err := page.Execute(&buf, d); err != nil {
		return nil, fmt.Errorf("report: render: %w", err)
	}
	return buf.Bytes(), nil
}

func reasonsText(by map[model.Reason]uint64) string {
	type rc struct {
		r model.Reason
		n uint64
	}
	var list []rc
	var total uint64
	for r, n := range by {
		if n > 0 {
			list = append(list, rc{r, n})
			total += n
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].n != list[j].n {
			return list[i].n > list[j].n
		}
		return list[i].r < list[j].r
	})
	var parts []string
	for i, x := range list {
		if i == 3 {
			break
		}
		parts = append(parts, fmt.Sprintf("%s %s", x.r.String(), fmtPct(float64(x.n)/float64(total))))
	}
	return strings.Join(parts, ", ")
}

var page = template.Must(template.New("report").Parse(pageTmpl))

const pageTmpl = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta http-equiv="Content-Security-Policy" content="{{.CSP}}">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow">
<meta name="referrer" content="no-referrer">
<title>{{.Title}}</title>
<style>{{.CSS}}</style>
</head>
<body>
<header id="header">
<p class="eyebrow">fyisp evidence report{{if .Redacted}} · redacted for sharing{{end}}</p>
<h1>{{.Title}}</h1>
<dl class="meta">
<dt>Range</dt><dd>{{.From}} → {{.To}} ({{.Span}})</dd>
<dt>Generated</dt><dd>{{.Now}}</dd>
{{- if .ISP}}<dt>ISP</dt><dd>{{.ISP}} <span class="note">(owner of the first public hop)</span></dd>{{end}}
{{- if .Version}}<dt>fyisp</dt><dd>version {{.Version}}</dd>{{end}}
</dl>
<div class="method">
<h3>How this was measured</h3>
{{range .Method}}<p>{{.}}</p>
{{end}}</div>
</header>

<section id="summary">
<h2>Summary</h2>
<p class="headline{{if .Good}} ok{{end}}">{{.Headline}}</p>
{{if .Findings}}<ul class="findings">{{range .Findings}}<li>{{.}}</li>{{end}}</ul>{{end}}
{{- if .Totals}}
<div class="tw"><table class="totals">
<thead><tr><th>Problem</th><th class="n">Incidents</th><th class="n">Total time</th><th class="n">Days affected</th><th class="n">Peak loss</th></tr></thead>
<tbody>{{range .Totals}}<tr><td><span class="k">{{.Swatch}}{{.Label}}</span></td><td class="n">{{.Count}}</td><td class="n">{{.Dur}}</td><td class="n">{{.Days}}</td><td class="n">{{.PeakLoss}}</td></tr>{{end}}</tbody>
</table></div>
{{- end}}
</section>

<section id="outages" class="brk">
<h2>Outage log</h2>
{{- if .Incidents}}
<p class="note">Every period in which fyisp's verdict was not “ok”, oldest first. Peak loss is the worst one-minute loss seen during the incident.{{if .IncOmitted}} The {{.IncOmitted}} shortest of {{.IncTotal}} incidents are not listed (they are included in the totals).{{end}}</p>
<div class="tw"><table class="log">
<thead><tr><th>Start (UTC)</th><th>End (UTC)</th><th class="n">Duration</th><th>Problem</th><th>What happened</th><th class="n">Peak loss</th><th>Affected</th></tr></thead>
<tbody>{{range .Incidents}}<tr><td class="t">{{.Start}}</td><td class="t">{{.End}}</td><td class="n">{{.Dur}}</td><td><span class="k">{{.Swatch}}{{.Label}}</span></td><td>{{.Summary}}</td><td class="n">{{.Peak}}</td><td class="aff">{{.Targets}}</td></tr>
{{end}}</tbody>
</table></div>
{{- else}}
<p class="empty">{{if .HasData}}No incidents: the verdict was “ok” for the whole range.{{else}}No data: fyisp has no measurements in this range.{{end}}</p>
{{- end}}
</section>

<section id="charts" class="brk">
<h2>Latency and loss</h2>
{{- if .HasData}}
<p class="note">Typical is the median latency, p95 the 95th percentile, both of {{.StatStep}} averages. Loss excludes time that was not measured.{{if .Base}} “Normal” is each target's median over the week before the report.{{end}}</p>
<div class="tw"><table class="overview">
<thead><tr><th>Group</th><th class="n">Targets</th><th class="n">Typical</th><th class="n">p95</th><th class="n">Loss</th><th class="n">vs normal</th></tr></thead>
<tbody>{{range .Overview}}<tr{{if .Hot}} class="hotrow"{{end}}><td>{{.Title}}{{if .Charted}} <span class="note">(chart below)</span>{{end}}</td><td class="n">{{.Targets}}</td>{{if .NoData}}<td class="n" colspan="4">no data</td>{{else}}<td class="n">{{.Typical}}</td><td class="n">{{.P95}}</td><td class="n">{{.Loss}}</td><td class="n">{{.Ratio}}</td>{{end}}</tr>
{{end}}</tbody>
</table></div>
{{range .Charts}}
<figure class="group">
<h3>{{.Title}}</h3>
<div class="cw">{{.Chart.SVG}}</div>
<figcaption>
<ul class="legend">{{range .Chart.Series}}<li>{{.Swatch}}{{.Label}}</li>{{end}}</ul>
{{- if or .Chart.Reasons .Chart.Kinds}}<ul class="legend">{{range .Chart.Kinds}}<li>{{.Swatch}}{{.Label}}</li>{{end}}{{range .Chart.Reasons}}<li>{{.Swatch}}{{.Label}}</li>{{end}}</ul>{{end}}
<p class="note">Lines: mean latency per {{.Chart.Step}}{{if .Chart.Band}}; shaded band: fastest to slowest probe{{end}}. Strip below: share of probes lost across all targets of the group (darker is more). Shaded columns: incidents.{{if .Chart.Omitted}} {{.Chart.Omitted}} more targets are in the table only.{{end}}{{if .Chart.Clipped}} {{.Chart.Clipped}}{{end}}</p>
</figcaption>
</figure>
<div class="tw"><table class="stats">
<thead><tr><th>Target</th><th>Probe</th><th class="n">Mean</th><th class="n">Typical</th><th class="n">p95</th><th class="n">Loss</th><th>Lost as</th>{{if .Base}}<th class="n">Normal</th><th class="n">vs normal</th>{{end}}</tr></thead>
<tbody>{{range .Rows}}<tr><td><span class="k">{{.Swatch}}{{.Name}}</span></td><td>{{.Kind}}</td>{{if .NoData}}<td class="n" colspan="{{if $.Base}}7{{else}}5{{end}}">no data</td>{{else}}<td class="n">{{.Mean}}</td><td class="n">{{.Median}}</td><td class="n">{{.P95}}</td><td class="n{{if .LossHot}} hot{{end}}">{{.Loss}}</td><td class="why">{{.Reasons}}</td>{{if $.Base}}<td class="n">{{.Normal}}</td><td class="n{{if .Hot}} hot{{end}}">{{.Ratio}}</td>{{end}}{{end}}</tr>
{{end}}</tbody>
</table></div>
{{end}}
{{- else}}
<p class="empty">No data: fyisp has no measurements in this range.</p>
{{- end}}
</section>
{{if .HasTrace}}
<section id="traces" class="brk">
<h2>Traceroute evidence</h2>
{{- if .Traces}}
<p class="note">Loss and latency at every router on the way to a target over the whole range. Loss that <strong>continues to the destination</strong> is real: packets are dropped there or before. Loss at one router only is that router limiting its own replies, which does not affect your traffic.{{if .Redacted}} Addresses inside your home and your ISP's private network are hidden, and your ISP's first public router is shown only as its /24 network.{{end}}</p>
{{range .Traces}}
<div class="trace">
<h3>{{.Target}}</h3>
<p class="note">{{.Why}}.</p>
{{- if .Hops}}
<div class="tw"><table class="hops">
<thead><tr><th class="n">Hop</th><th>Address</th><th>Owner</th><th class="n">Loss</th><th class="n">Mean</th><th class="n">p95</th><th>Loss is</th></tr></thead>
<tbody>{{range .Hops}}<tr{{if .Class}} class="{{.Class}}"{{end}}><td class="n">{{.Hop}}</td><td class="addr">{{if .Addr}}{{.Addr}}{{end}}{{if .Note}} <span class="note">{{.Note}}</span>{{end}}{{if .RDNS}}<br><span class="note">{{.RDNS}}</span>{{end}}</td><td>{{.Owner}}</td><td class="n">{{.Loss}}</td><td class="n">{{.Mean}}</td><td class="n">{{.P95}}</td><td class="why">{{.Verdict}}</td></tr>
{{end}}</tbody>
</table></div>
{{- end}}
{{- if .Changes}}
<h4>Route changes (newest first)</h4>
<div class="tw"><table class="changes">
<thead><tr><th>When (UTC)</th><th class="n">From hop</th><th>Before</th><th>After</th></tr></thead>
<tbody>{{range .Changes}}<tr><td class="t">{{.At}}</td><td class="n">{{.FirstDiff}}</td><td class="addr">{{.From}}</td><td class="addr">{{.To}}</td></tr>
{{end}}</tbody>
</table></div>
{{- if .More}}<p class="note">{{.More}} older changes are not listed.</p>{{end}}
{{- else}}
<p class="note">The route did not change in this range.</p>
{{- end}}
</div>
{{end}}
{{- else}}
<p class="empty">No traceroute data in this range.</p>
{{- end}}
</section>
{{end}}
{{- if .HasNotes}}
<section id="notes">
<h2>Notes</h2>
{{- if .Notes}}
<div class="tw"><table class="notes">
<thead><tr><th>When (UTC)</th><th>Note</th></tr></thead>
<tbody>{{range .Notes}}<tr><td class="t">{{.When}}</td><td>{{.Text}}{{if .Private}} <span class="note">(private note)</span>{{end}}</td></tr>
{{end}}</tbody>
</table></div>
{{- else}}
<p class="empty">No {{if .Redacted}}public {{end}}notes in this range.</p>
{{- end}}
</section>
{{- end}}

<footer id="footer">
<h2>How to read this report</h2>
<dl class="kinds">
<dt>ISP problem</dt><dd>Your router answered normally, but the first router on your ISP's side did not: the fault is in your ISP's access network.</dd>
<dt>Internet problem beyond your ISP</dt><dd>Your router and your ISP's first router were fine, but the big anycast resolvers (Cloudflare, Google, Quad9) and most services failed: your ISP's upstream links or peering.</dd>
<dt>Home network problem</dt><dd>Your own router did not answer reliably: Wi-Fi, cables or the router. Not your ISP's fault.</dd>
<dt>DNS problem</dt><dd>The network worked but name lookups failed widely: usually the router's or the ISP's DNS resolver.</dd>
<dt>Some services down</dt><dd>Only some services failed: their problem, not your connection's.</dd>
<dt>No network</dt><dd>This device had no working connection at all.</dd>
</dl>
<p>Loss on an inner link shows on every layer beyond it, so each problem is blamed on the innermost layer that lost probes; an outer layer is blamed only when it loses significantly more. Latency is the time to the first response byte (HTTPS), to connect (TCP) or of the echo reply (ICMP). “Not measured” time (fyisp stopped, the computer asleep) is never counted as loss.</p>
<p class="gen">Generated by fyisp{{if .Version}} {{.Version}}{{end}} on {{.Now}}. This report is a static document: it contains no scripts and loads nothing from the network.</p>
</footer>
</body>
</html>
`

const css = `:root{--ink:#0b0b0b;--ink-2:#52514e;--muted:#6f6e69;--line:#e1e0d9;--axis:#c3c2b7;--surface:#f4f3ef;--bad:#cc2f2f;--bad-bg:#fbeaea;--good:#188a2e;--good-bg:#eaf5ec;color-scheme:light}
*{box-sizing:border-box}
html{background:#fff}
body{margin:0 auto;max-width:980px;padding:32px 24px 48px;background:#fff;color:var(--ink);font:14px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,"Helvetica Neue",Arial,sans-serif;-webkit-print-color-adjust:exact;print-color-adjust:exact;overflow-wrap:break-word}
h1{font-size:28px;line-height:1.2;margin:0 0 12px}
h2{font-size:20px;line-height:1.3;margin:0 0 12px;padding-bottom:6px;border-bottom:2px solid var(--ink)}
h3{font-size:15px;margin:22px 0 6px}
h4{font-size:13px;margin:14px 0 4px;color:var(--ink-2)}
p{margin:0 0 8px}
.eyebrow{text-transform:uppercase;letter-spacing:.08em;font-size:11px;font-weight:600;color:var(--muted);margin:0 0 4px}
dl.meta{display:grid;grid-template-columns:max-content 1fr;gap:2px 16px;margin:0 0 16px}
dl.meta dt{color:var(--muted)}
dl.meta dd{margin:0}
.method{background:var(--surface);border-radius:6px;padding:4px 16px 8px;font-size:13px;color:var(--ink-2)}
.method h3{margin-top:10px;color:var(--ink)}
section{margin-top:36px}
.headline{font-size:18px;font-weight:600;line-height:1.4;padding:12px 16px;margin:0 0 12px;background:var(--bad-bg);border-left:4px solid var(--bad);border-radius:0 6px 6px 0}
.headline.ok{background:var(--good-bg);border-color:var(--good)}
ul.findings{margin:0 0 16px;padding-left:20px}
ul.findings li{margin:2px 0}
.tw{overflow-x:auto;margin:0 0 12px}
table{border-collapse:collapse;width:100%;font-size:12.5px;font-variant-numeric:tabular-nums}
th,td{text-align:left;padding:5px 8px;border-bottom:1px solid var(--line);vertical-align:top}
th{font-weight:600;color:var(--ink-2);border-bottom:1.5px solid var(--axis);white-space:nowrap}
.n{text-align:right;white-space:nowrap}
td.t{white-space:nowrap}
td.addr{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:11.5px}
td.why,td.aff{color:var(--ink-2)}
thead{display:table-header-group}
tr{break-inside:avoid}
.hot{color:var(--bad);font-weight:600}
tr.hotrow td:first-child{font-weight:600}
tr.real td{background:var(--bad-bg)}
tr.ratelimit td.why{color:var(--muted)}
.k{display:inline-flex;align-items:center;gap:2px;white-space:nowrap}
.cw{overflow-x:auto}
.sw{width:18px;height:10px;flex:none;vertical-align:-1px;margin-right:4px}
figure{margin:18px 0 8px;break-inside:avoid}
figure h3{margin-top:0}
svg.chart{display:block;width:100%;height:auto}
svg.chart text{font:10px system-ui,-apple-system,"Segoe UI",Roboto,Arial,sans-serif}
ul.legend{list-style:none;padding:0;margin:4px 0;display:flex;flex-wrap:wrap;gap:2px 16px;font-size:12px;color:var(--ink-2)}
ul.legend li{display:inline-flex;align-items:center}
.note{color:var(--muted);font-size:12px}
.empty{padding:12px 16px;background:var(--surface);border-radius:6px;color:var(--ink-2)}
.trace{break-inside:auto}
footer{margin-top:40px;padding-top:4px;color:var(--ink-2);font-size:12.5px;break-inside:avoid}
footer h2{color:var(--ink)}
dl.kinds{display:grid;grid-template-columns:max-content 1fr;gap:4px 16px;margin:0 0 12px}
dl.kinds dt{font-weight:600;color:var(--ink)}
dl.kinds dd{margin:0}
.gen{margin-top:16px;padding-top:8px;border-top:1px solid var(--line);color:var(--muted)}
@media (max-width:600px){body{padding:20px 16px 32px}svg.chart{min-width:640px}table.log{min-width:840px}table.stats{min-width:780px}table.hops,table.changes{min-width:660px}td.why{min-width:10em}h1{font-size:22px}dl.kinds{grid-template-columns:1fr}dl.kinds dd{margin-bottom:6px}}
@page{margin:14mm 12mm}
@media print{body{max-width:none;padding:0;font-size:10.5pt}section{margin-top:0}section.brk{break-before:page}#summary{margin-top:24px}#notes,footer{margin-top:28px}h2,h3,h4{break-after:avoid}.tw{overflow:visible}table{font-size:9pt}}
`

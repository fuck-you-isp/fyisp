package web

// The JSON of /api/home (docs/HOME-API.md): the Home's answer
// (part=now) and a tile's panel (part=tile). Every sentence is written by
// home_copy.go; the client lays them out and formats only bar times,
// scale ticks and "Checked N ago". Times are Unix milliseconds.

// HomeNow is part=now.
type HomeNow struct {
	At            int64         `json:"at"`
	LastCheck     int64         `json:"last_check"` // the newest measured sample; 0: none yet
	Stale         bool          `json:"stale"`      // last_check is staleAfter (2 min) or more old
	WatchingSince int64         `json:"watching_since"`
	Footer        string        `json:"footer"`
	Learning      *HomeLearning `json:"learning"`
	Answer        HomeAnswer    `json:"answer"`
	Path          []HomeHop     `json:"path"`      // always 3: router, provider, internet
	PathHint      string        `json:"path_hint"` // "" with the Wi-Fi wording
	Tiles         []HomeTile    `json:"tiles"`     // always 11, in profile.Tiles' order
	Speed         HomeSpeed     `json:"speed"`
	Problems      HomeProblems  `json:"problems"`
}

// HomeLearning is the first day's note.
type HomeLearning struct {
	Text string `json:"text"`
}

// HomeAnswer is the answer card. Key changes when the card's meaning
// does (the client announces only then); Headline and Sub never change
// under one key.
type HomeAnswer struct {
	Key      string   `json:"key"`
	Tone     string   `json:"tone"` // ok, warn, bad, stale, checking, info
	Icon     string   `json:"icon"` // good, slow, down, none, info
	Tab      string   `json:"tab"`  // the tab title; the client adds " – fyisp"
	Headline string   `json:"headline"`
	Sub      string   `json:"sub"`
	Day      *HomeDay `json:"day"` // nil: no line (checking, learning)
}

// HomeDay is the "Last 24 hours" line; Link is the part of Text that
// links to the problems ("" for none).
type HomeDay struct {
	Text string `json:"text"`
	Link string `json:"link"`
}

// HomeHop is one node of the connection strip.
type HomeHop struct {
	ID    string `json:"id"` // router, provider, internet
	Label string `json:"label"`
	State string `json:"state"` // good, slow, down, unknown
	Sub   string `json:"sub"`
}

// HomeTile is one tile.
type HomeTile struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Kind        string    `json:"kind"`  // call, app, stream
	State       string    `json:"state"` // good, slow, down, none, off
	Word        string    `json:"word"`  // Good, Slow, Choppy, Down, No data, Not checked
	Since       *int64    `json:"since"` // null when Good, no data or off
	Note        string    `json:"note"`  // "since 10:05 am", "Dropped 40 s at 10:14 am" or ""
	Aria        string    `json:"aria"`
	Reason      string    `json:"reason"`
	EnableHint  string    `json:"enable_hint,omitempty"` // off tiles
	BarsSummary string    `json:"bars_summary"`
	Bars        []HomeBar `json:"bars"` // 24 × 5 min, the last the current one; [] when off
}

// HomeBar is one 5-minute bar.
type HomeBar struct {
	From  int64  `json:"from"`
	State string `json:"state"` // good, slow, down, none
	Note  string `json:"note"`  // "" when the word says it
}

// HomeSpeed is the speed line.
type HomeSpeed struct {
	Available bool              `json:"available"`
	Latest    *HomeSpeedResult  `json:"latest"`
	EmptyText string            `json:"empty_text"`
	Running   *HomeSpeedRunning `json:"running"`
}

// HomeSpeedResult is the latest speed test.
type HomeSpeedResult struct {
	At   int64  `json:"at"`
	Text string `json:"text"`
	Note string `json:"note"`
}

// HomeSpeedUsual is the usual speed: the median of the OK tests of 7 days,
// the latest left out, once 3 or more precede it.
type HomeSpeedUsual struct {
	DownMbps, UpMbps float64
}

// HomeSpeedRunning is a test running now (from any source).
type HomeSpeedRunning struct {
	Entry uint64 `json:"entry"`
}

// HomeProblems are the problems of the last 7 days.
type HomeProblems struct {
	Summary string        `json:"summary"`
	Items   []HomeProblem `json:"items"` // newest first
}

// HomeProblem is one problem.
type HomeProblem struct {
	ID        string `json:"id"`
	From      int64  `json:"from"`
	To        int64  `json:"to"` // 0 while ongoing
	Ongoing   bool   `json:"ongoing"`
	Level     string `json:"level"` // slow, down
	When      string `json:"when"`
	What      string `json:"what"`
	Cause     string `json:"cause"` // follows "Likely cause: "
	CauseKind string `json:"cause_kind"`
	Evidence  string `json:"evidence"`
}

// HomeTileDetail is part=tile: the header, rendered live, and the chart
// of the last 24 hours (cached per 5-minute bucket).
type HomeTileDetail struct {
	At       int64        `json:"at"`
	ID       string       `json:"id"`
	State    string       `json:"state"`
	Word     string       `json:"word"`
	Reason   string       `json:"reason"`
	Coverage string       `json:"coverage"`
	Metrics  []HomeMetric `json:"metrics"`
	// Legend names the chart's line when it isn't the app's own ("" : the
	// app's): "How quickly the general call path answers (lower is better)".
	Legend string `json:"legend,omitempty"`
	homeChart
}

// homeChart is part=tile's chart block.
type homeChart struct {
	From    int64      `json:"from"`
	Step    int64      `json:"step"`
	Ms      []*float64 `json:"ms"`       // 288; null: no data
	UsualMs *float64   `json:"usual_ms"` // null while learning
	Bands   []HomeBand `json:"bands"`
	Text    string     `json:"text"` // the chart's text alternative
}

// HomeMetric is one figure of the panel.
type HomeMetric struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Limit string `json:"limit,omitempty"`
	Bad   *bool  `json:"bad,omitempty"`
}

// HomeBand is a shaded span of the chart.
type HomeBand struct {
	From int64  `json:"from"`
	To   int64  `json:"to"`
	Kind string `json:"kind"` // problem, off
	Text string `json:"text"`
}

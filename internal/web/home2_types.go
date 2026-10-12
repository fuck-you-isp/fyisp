package web

// The JSON of /api/home2 (docs/HOME-API.md, v2): the new Home's answer
// (part=now) and a tile's panel (part=tile). Every sentence is written by
// home2_copy.go; the client lays them out and formats only bar times,
// scale ticks and "Checked N ago". Times are Unix milliseconds.

// Home2Now is part=now.
type Home2Now struct {
	V             int            `json:"v"` // 2
	At            int64          `json:"at"`
	LastCheck     int64          `json:"last_check"` // the newest measured sample; 0: none yet
	Stale         bool           `json:"stale"`      // last_check is staleAfter (2 min) or more old
	WatchingSince int64          `json:"watching_since"`
	Footer        string         `json:"footer"`
	Learning      *Home2Learning `json:"learning"`
	Monitored     int            `json:"monitored"` // tiles not off
	Answer        Home2Answer    `json:"answer"`
	Path          []Home2Hop     `json:"path"`      // always 3: router, provider, internet
	PathHint      string         `json:"path_hint"` // "" with the Wi-Fi wording
	Tiles         []Home2Tile    `json:"tiles"`     // always 11, in profile.Tiles' order
	Speed         Home2Speed     `json:"speed"`
	Problems      Home2Problems  `json:"problems"`
}

// Home2Learning is the first day's note.
type Home2Learning struct {
	Until int64  `json:"until"`
	Text  string `json:"text"`
}

// Home2Answer is the answer card. Key changes when the card's meaning
// does (the client announces only then); Headline and Sub never change
// under one key.
type Home2Answer struct {
	Key      string    `json:"key"`
	Tone     string    `json:"tone"` // ok, warn, bad, stale, checking, info
	Icon     string    `json:"icon"` // good, slow, down, none, info
	Tab      string    `json:"tab"`  // the tab title; the client adds " – fyisp"
	Headline string    `json:"headline"`
	Sub      string    `json:"sub"`
	Day      *Home2Day `json:"day"` // nil: no line (checking, learning)
}

// Home2Day is the "Last 24 hours" line; Link is the part of Text that
// links to the problems ("" for none).
type Home2Day struct {
	Text string `json:"text"`
	Link string `json:"link"`
}

// Home2Hop is one node of the connection strip.
type Home2Hop struct {
	ID    string `json:"id"` // router, provider, internet
	Label string `json:"label"`
	State string `json:"state"` // good, slow, down, unknown
	Sub   string `json:"sub"`
}

// Home2Tile is one tile.
type Home2Tile struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Kind        string     `json:"kind"`  // call, app, stream
	State       string     `json:"state"` // good, slow, down, none, off
	Word        string     `json:"word"`  // Good, Slow, Choppy, Down, No data, Not checked
	Since       *int64     `json:"since"` // null when Good, no data or off
	Note        string     `json:"note"`  // "since 10:05 am", "Dropped 40 s at 10:14 am" or ""
	Aria        string     `json:"aria"`
	Reason      string     `json:"reason"`
	EnableHint  string     `json:"enable_hint,omitempty"` // off tiles
	BarsSummary string     `json:"bars_summary"`
	Bars        []Home2Bar `json:"bars"` // 24 × 5 min, the last the current one; [] when off
}

// Home2Bar is one 5-minute bar.
type Home2Bar struct {
	From  int64  `json:"from"`
	State string `json:"state"` // good, slow, down, none
	Note  string `json:"note"`  // "" when the word says it
}

// Home2Speed is the speed line.
type Home2Speed struct {
	Available bool               `json:"available"`
	Latest    *Home2SpeedResult  `json:"latest"`
	EmptyText string             `json:"empty_text"`
	Usual     *Home2SpeedUsual   `json:"usual"` // the median of the OK tests of 7 days (3 or more), the latest left out
	Running   *Home2SpeedRunning `json:"running"`
	// CanControl: this request may start a test (no admin token needed).
	CanControl bool `json:"can_control"`
}

// Home2SpeedResult is the latest speed test.
type Home2SpeedResult struct {
	At       int64   `json:"at"`
	DownMbps float64 `json:"down_mbps"`
	UpMbps   float64 `json:"up_mbps"`
	Text     string  `json:"text"`
	Note     string  `json:"note"`
}

// Home2SpeedUsual is the usual speed.
type Home2SpeedUsual struct {
	DownMbps float64 `json:"down_mbps"`
	UpMbps   float64 `json:"up_mbps"`
}

// Home2SpeedRunning is a test running now (from any source).
type Home2SpeedRunning struct {
	Entry       uint64 `json:"entry"`
	ExpectedEnd int64  `json:"expected_end"`
}

// Home2Problems are the problems of the last 7 days.
type Home2Problems struct {
	Summary string         `json:"summary"`
	Items   []Home2Problem `json:"items"` // newest first
}

// Home2Problem is one problem.
type Home2Problem struct {
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

// Home2TileDetail is part=tile: the header, rendered live, and the chart
// of the last 24 hours (cached per 5-minute bucket).
type Home2TileDetail struct {
	V        int           `json:"v"`
	At       int64         `json:"at"`
	ID       string        `json:"id"`
	State    string        `json:"state"`
	Word     string        `json:"word"`
	Reason   string        `json:"reason"`
	Coverage string        `json:"coverage"`
	Metrics  []Home2Metric `json:"metrics"`
	home2Chart
}

// home2Chart is part=tile's chart block.
type home2Chart struct {
	From    int64       `json:"from"`
	Step    int64       `json:"step"`
	Ms      []*float64  `json:"ms"`       // 288; null: no data
	UsualMs *float64    `json:"usual_ms"` // null while learning
	Bands   []Home2Band `json:"bands"`
	Text    string      `json:"text"` // the chart's text alternative
}

// Home2Metric is one figure of the panel.
type Home2Metric struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Limit string `json:"limit,omitempty"`
	Bad   *bool  `json:"bad,omitempty"`
}

// Home2Band is a shaded span of the chart.
type Home2Band struct {
	From int64  `json:"from"`
	To   int64  `json:"to"`
	Kind string `json:"kind"` // problem, off
	Text string `json:"text"`
}

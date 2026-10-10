package web

// /api/home: the Home view's answer, tiles and history, rolled up on the
// server so the local page, the public link and reports say the same thing.
// The contract is docs/HOME-API.md; keep the two in sync.
//
// Times are Unix milliseconds. Copy strings never contain a time or a
// number of milliseconds: the client formats every time from the *_at /
// from / to fields in the viewer's locale.

// Answer and tile levels: the severity that picks the icon and colour.
const (
	levelGood     = "good"
	levelWarn     = "warn"
	levelBad      = "bad"
	levelInfo     = "info"     // a known, long-standing condition (a quiet UDP block): neutral, no alarm
	levelChecking = "checking" // warming up or not enough data yet
)

// Who is at fault, in Home's vocabulary.
const (
	whoNone        = "none"
	whoHome        = "home"        // verdict lan: Wi-Fi, cable or router
	whoProvider    = "provider"    // verdict isp
	whoBeyond      = "beyond"      // verdict upstream: further out than the provider's first hop
	whoDNS         = "dns"         // verdict dns: name lookups
	whoApp         = "app"         // verdict service on a Home tile, or one call app worse than the path
	whoCallTraffic = "calltraffic" // verdicts udp_blocked, stun_blocked, udp_degraded; a bad call path score
	whoOffline     = "offline"     // verdict no_network
	whoUnknown     = "unknown"     // no verdict source
)

// Confidence tiers for blame (docs/HOME-API.md, "Confidence").
const (
	confSure   = "sure"   // the layer verdict is corroborated by impact the user notices
	confLikely = "likely" // the path layers alone
	confWeak   = "weak"   // only a router or provider hop is slow to answer checks; nothing the user uses is affected
)

// Voices: "you" on the owner's dashboard, "public" (third person) on the
// share link.
const (
	voiceYou    = "you"
	voicePublic = "public"
)

// HomeLive is /api/home?part=live.
type HomeLive struct {
	Part   string     `json:"part"` // "live"
	Voice  string     `json:"voice"`
	At     int64      `json:"at"`
	Answer HomeAnswer `json:"answer"`
	Path   []HomeNode `json:"path"`
	Tiles  []HomeTile `json:"tiles"`
	// Other counts unhealthy targets that belong to no Home tile (e.g. a
	// cloud region). Shown as a muted line, never an alarm, unless the
	// profile has nothing else (see the Other tile).
	Other *HomeOther `json:"other,omitempty"`
	Calls *HomeCalls `json:"calls,omitempty"`
	Apps  []HomeApp  `json:"apps"`
	Steps []HomeStep `json:"steps,omitempty"` // warming up only
}

// HomeAnswer is the one answer at the top of Home.
type HomeAnswer struct {
	Level      string `json:"level"`
	Who        string `json:"who"`
	Confidence string `json:"confidence,omitempty"` // with who home, provider, beyond, dns
	// Kind is the raw verdict kind ("unknown" without a verdict source),
	// for Details links; Calls the call state that fed the answer.
	Kind  string `json:"kind"`
	Calls string `json:"calls,omitempty"`
	App   string `json:"app,omitempty"`   // app id with who app ("teams", ...)
	Since int64  `json:"since,omitempty"` // when the current problem started; 0 when none or unknown
	// Copy. Headline is one short sentence; Cause says where the problem
	// is; Hint is an optional third line; Try lists what the user can do
	// (empty in the public voice); Fact is one labelled fact in words
	// about the worst minute, at FactAt.
	Headline string   `json:"headline"`
	Cause    string   `json:"cause,omitempty"`
	Hint     string   `json:"hint,omitempty"`
	Try      []string `json:"try"`
	Fact     string   `json:"fact,omitempty"`
	FactAt   int64    `json:"fact_at,omitempty"`
	// Announce is the short text a screen reader should speak when Level
	// or Who changes (the client announces only on change).
	Announce string `json:"announce"`
}

// HomeNode is one step of the path strip: device, router, provider,
// internet, apps.
type HomeNode struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	State   string `json:"state"` // ok | warn | bad | unknown | finding
	Words   string `json:"words"` // state in words: "OK", "Dropping some data", "Not answering", ...
	Blamed  bool   `json:"blamed,omitempty"`
	Tooltip string `json:"tooltip,omitempty"`
}

// HomeTile is one of Calls, Work apps, Websites and Other
// services. Tiles without targets are left out.
type HomeTile struct {
	ID    string `json:"id"` // calls | work | web | other (profile.HomeCategory)
	Title string `json:"title"`
	Level string `json:"level"`
	// State: for calls the call state (good, fair, poor, unusable,
	// blocked, not_enough_data, server_not_answering); for the others
	// working, mostly, slow, down or checking.
	State string `json:"state"`
	Label string `json:"label"` // the state in words
	Note  string `json:"note,omitempty"`
	// Names are the display names in the tile (at most 3, then More);
	// Affected the unhealthy ones. OK and Total count targets (calls: apps).
	Names    []string `json:"names"`
	More     int      `json:"more,omitempty"`
	Affected []string `json:"affected,omitempty"`
	OK       int      `json:"ok"`
	Total    int      `json:"total"`
	// Details is the Details view hash to open for this tile.
	Details string `json:"details"`
}

type HomeOther struct {
	Affected int    `json:"affected"`
	Total    int    `json:"total"`
	Line     string `json:"line"`
}

// HomeCalls is the call path in words for the Calls tile's detail.
type HomeCalls struct {
	State      string `json:"state"`
	Delay      string `json:"delay,omitempty"`      // very_short | short | noticeable | long
	Dropouts   string `json:"dropouts,omitempty"`   // none | rare | some | frequent
	Smoothness string `json:"smoothness,omitempty"` // steady | bit_uneven | uneven
	Words      string `json:"words,omitempty"`      // "Delay very short · Dropouts none · Smoothness steady"
	WorstAt    int64  `json:"worst_at,omitempty"`
}

// HomeApp is one row of "Your call apps".
type HomeApp struct {
	App   string `json:"app"`
	Title string `json:"title"`
	Level string `json:"level"`
	// State: a call state when its call path is measured (Meet; Teams with
	// the opt-in relay); "reachable" or "not_answering" when only its
	// website is (Teams by default, Zoom, Slack, ...).
	State string `json:"state"`
	Label string `json:"label"`
	Note  string `json:"note,omitempty"`
}

// HomeStep is a warm-up progress step.
type HomeStep struct {
	ID    string `json:"id"` // router | provider | internet | apps
	Label string `json:"label"`
	Done  bool   `json:"done"`
}

// HomeHistory is /api/home?part=history.
type HomeHistory struct {
	Part  string `json:"part"` // "history"
	Voice string `json:"voice"`
	At    int64  `json:"at"`
	TZ    int    `json:"tz"`   // minutes east of UTC, as used (rounded)
	Span  string `json:"span"` // d | w
	From  int64  `json:"from"`
	To    int64  `json:"to"`
	// FirstData is the first measurement in the store (0: none yet); days
	// before it have no record.
	FirstData int64          `json:"first_data,omitempty"`
	Days      []HomeDay      `json:"days"`
	Summary   string         `json:"summary"`
	Counts    HomeCounts     `json:"counts"`
	Problems  []HomeProblem  `json:"problems"`
	Work      *HomeWorkCalls `json:"work,omitempty"`
}

// HomeDay is one local day of the timeline, oldest first.
type HomeDay struct {
	Date     string     `json:"date"` // local YYYY-MM-DD
	From     int64      `json:"from"`
	To       int64      `json:"to"`                  // the day's end, or now for today
	WorkFrom int64      `json:"work_from,omitempty"` // Mon-Fri 09-17 local; 0 on weekends
	WorkTo   int64      `json:"work_to,omitempty"`
	Runs     []HomeRun  `json:"runs"`
	Off      []HomeSpan `json:"off"`
	NoRecord bool       `json:"no_record,omitempty"` // the whole day is before FirstData
}

// HomeRun is a bad or warn stretch on the timeline.
type HomeRun struct {
	From  int64  `json:"from"`
	To    int64  `json:"to"`
	Level string `json:"level"` // warn | bad
	Who   string `json:"who"`
	Kind  string `json:"kind,omitempty"` // verdict kind for incidents; call state for call-only runs
	Label string `json:"label"`          // who in words: "Your provider", "Home network", ...
	// Source: "incident" (the outage log; Incident is its id) or "calls"
	// (bad call minutes with no incident, today only).
	Source   string `json:"source"`
	Incident int64  `json:"incident,omitempty"`
	Ongoing  bool   `json:"ongoing,omitempty"`
}

type HomeSpan struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
}

// HomeCounts counts incidents in the span by who.
type HomeCounts struct {
	Problems    int `json:"problems"`
	Home        int `json:"home"`
	Provider    int `json:"provider"`
	Beyond      int `json:"beyond"`
	DNS         int `json:"dns"`
	App         int `json:"app"`
	CallTraffic int `json:"calltraffic"`
	Offline     int `json:"offline"`
}

// HomeProblem is a row of the problem table (the outage log in Home's
// words), newest first.
type HomeProblem struct {
	ID       int64   `json:"id"`
	From     int64   `json:"from"`
	To       int64   `json:"to,omitempty"` // 0 while ongoing
	Ongoing  bool    `json:"ongoing,omitempty"`
	Kind     string  `json:"kind"`
	Who      string  `json:"who"`
	Where    string  `json:"where"` // "Provider network (past the router)", ...
	PeakLoss float64 `json:"peak_loss"`
	Details  string  `json:"details"` // Details view hash zoomed to it
}

// HomeWorkCalls is today's call quality in working hours (span d only).
type HomeWorkCalls struct {
	Measured int     `json:"measured"` // scored minutes in working hours so far
	Bad      int     `json:"bad"`      // of them poor or unusable
	Good     float64 `json:"good"`     // share of measured minutes that were good or fair
}

// HomeAt is /api/home?part=at&at=<ms>: what happened around a picked time
// (at ±15 minutes, up to now).
type HomeAt struct {
	Part  string `json:"part"` // "at"
	Voice string `json:"voice"`
	At    int64  `json:"at"`   // server time of the answer
	Time  int64  `json:"time"` // the picked time
	From  int64  `json:"from"`
	To    int64  `json:"to"`
	// Measured: the call path was scored in at least one minute of the
	// window; Calls is then the worst minute's call state, and BadFrom and
	// BadTo span its minutes when it was not good.
	Measured bool   `json:"measured"`
	Calls    string `json:"calls,omitempty"`
	BadFrom  int64  `json:"bad_from,omitempty"`
	BadTo    int64  `json:"bad_to,omitempty"`
	Level    string `json:"level"`
	Who      string `json:"who"`
	Kind     string `json:"kind,omitempty"` // the incident's verdict kind, when one explains it
	Headline string `json:"headline"`
	Cause    string `json:"cause"`
	Fact     string `json:"fact,omitempty"`
	FactAt   int64  `json:"fact_at,omitempty"`
}

package speedtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/fuck-you-isp/fyisp/internal/profile"
)

// The speed-test catalog (internal/profile/catalog/iperf3): one file per
// provider listing its public iperf3 servers, and regions.yml, the regions
// tested in mode "regions" with their fallback chains. Data, changed by PR.
//
//	provider: leaseweb
//	display: Leaseweb                 # the label's provider part
//	sources: [https://...]            # required
//	servers:
//	  - id: Leaseweb-NewYork          # identity of stored results: never rename
//	    label: Leaseweb New York      # optional, default "<display> <city>"
//	    provider: Eranium             # optional, overrides display (shared files)
//	    host: speedtest.nyc1.us.leaseweb.net
//	    ports: 5201-5210              # one iperf3 instance per port, at most 64 ("5201": single port)
//	    city: New York
//	    metro: new-york               # servers of one metro back each other up
//	    country: US                   # ISO 3166-1 alpha-2
//	    geo: na                       # na sa eu me af as oc
//	    capacity_gbps: 10             # optional: the server's published link capacity
//	    verified: {date: 2026-10-10, tcp: true, iperf3: true}
//
// regions.yml also lists the removed servers' IDs (removed:). A server
// leaves the catalog by moving its ID there: at start, its stored tests
// become bare windows (Store.RetireSpeedTestServers), and naming it in a
// flag only warns.

// Server is one public iperf3 server: one iperf3 instance per port of
// PortMin..PortMax, each running one test at a time.
type Server struct {
	ID       string // identity of its stored results
	Label    string // provider and city
	Provider string // display name
	Host     string
	PortMin  int
	PortMax  int
	City     string
	Metro    string
	Country  string
	Geo      string
	// CapacityMbps is the published link capacity (0: unknown): a result
	// within 10% of it measured the server, not the line.
	CapacityMbps float64
	// Custom is a user-supplied server (--speedtest-server host:port). LAN:
	// it is on the user's own network (a private address); the 15-minute
	// floor does not apply when every server is one, and it is only ever
	// dialled at a private address.
	Custom bool
	LAN    bool
}

// PublicIPv4 reports a public unicast IPv4 address: not private, CGNAT,
// loopback, link-local, multicast, or any other IANA special-purpose block.
func PublicIPv4(a netip.Addr) bool {
	a = a.Unmap()
	if !a.Is4() || !a.IsGlobalUnicast() || a.IsPrivate() {
		return false
	}
	for _, p := range specialIPv4 {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// specialIPv4 is the IANA IPv4 special-purpose address registry's blocks
// that are not globally reachable (RFC 6890 and successors).
var specialIPv4 = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8",          // this network
		"10.0.0.0/8",         // private
		"100.64.0.0/10",      // shared address space (CGNAT)
		"127.0.0.0/8",        // loopback
		"169.254.0.0/16",     // link-local
		"172.16.0.0/12",      // private
		"192.0.0.0/24",       // IETF protocol assignments
		"192.0.2.0/24",       // TEST-NET-1
		"192.31.196.0/24",    // AS112-v4
		"192.52.193.0/24",    // AMT
		"192.88.99.0/24",     // deprecated 6to4 relay anycast
		"192.168.0.0/16",     // private
		"192.175.48.0/24",    // direct delegation AS112
		"198.18.0.0/15",      // benchmarking
		"198.51.100.0/24",    // TEST-NET-2
		"203.0.113.0/24",     // TEST-NET-3
		"224.0.0.0/4",        // multicast
		"240.0.0.0/4",        // reserved
		"255.255.255.255/32", // limited broadcast
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// PrivateIPv4 reports an address on the user's own networks: RFC 1918,
// CGNAT (100.64/10), link-local or loopback.
func PrivateIPv4(a netip.Addr) bool {
	a = a.Unmap()
	return a.Is4() && (a.IsPrivate() || cgnat.Contains(a) || a.IsLinkLocalUnicast() || a.IsLoopback())
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// check vets the address a test of s dials: catalog servers must be
// public (a rebound or hijacked name must not aim 8 streams into the LAN),
// the user's LAN servers private (they have no interval floor); the user's
// other servers may be anywhere.
func (s Server) check(a netip.Addr) error {
	switch {
	case !s.Custom && !PublicIPv4(a):
		return fmt.Errorf("%s resolves to %s, not a public address", s.Host, a)
	case s.LAN && !PrivateIPv4(a):
		return fmt.Errorf("%s resolves to %s, not on your network", s.Host, a)
	}
	return nil
}

func (s Server) ports() int { return s.PortMax - s.PortMin + 1 }

// SinglePort reports a server with one port: busy means move on at once.
func (s Server) SinglePort() bool { return s.ports() == 1 }

// Region is a region of mode "regions".
type Region struct {
	ID    string
	Title string
	// Servers are one metro's servers: each (region, hour of day) rotates
	// through their providers day by day; the others are the same-metro
	// fallback. Nearby are servers in a nearby city, tried last.
	Servers []string
	Nearby  []string
	// Others are the region's servers in no chain (sweeps and custom lists
	// still test them): they name the region a server belongs to.
	Others  []string
	Default bool // tested unless opted out; false: opt-in
}

// Fallback levels of a test (model.SpeedTest.FallbackLevel).
const (
	FallbackNone      = "none"
	FallbackSameMetro = "same_metro"
	FallbackNearby    = "nearby_city"
)

// Step is one server of a slot's chain, with its fallback level.
type Step struct {
	Server Server
	Level  string
}

// Providers are the region's providers, in the order of Servers.
func (r Region) Providers(c *Catalog) []string {
	var out []string
	for _, id := range r.Servers {
		if s, ok := c.Server(id); ok && !slices.Contains(out, s.Provider) {
			out = append(out, s.Provider)
		}
	}
	return out
}

// Rotation is the provider index for a (day, hour) of the region: day
// number plus hour plus the install's seed, modulo the providers, so every
// hour of the day sees every provider over that many days, and installs
// don't all start with the same one.
func Rotation(day, hour int64, seed uint32, providers int) int {
	if providers <= 1 {
		return 0
	}
	return int((day + hour + int64(seed)) % int64(providers))
}

// Chain is the region's servers to try at t (UTC day and hour): the
// rotation's provider first (its servers in list order), then the other
// same-metro servers in list order from there, then the nearby ones.
func (r Region) Chain(c *Catalog, t time.Time, seed uint32) []Step {
	provs := r.Providers(c)
	if len(provs) == 0 {
		return nil
	}
	u := t.UTC()
	first := provs[Rotation(u.Unix()/86400, int64(u.Hour()), seed, len(provs))]
	var out []Step
	add := func(id, level string) {
		if s, ok := c.Server(id); ok && !slices.ContainsFunc(out, func(x Step) bool { return x.Server.ID == id }) {
			out = append(out, Step{Server: s, Level: level})
		}
	}
	for _, id := range r.Servers {
		if s, _ := c.Server(id); s.Provider == first {
			add(id, FallbackNone)
		}
	}
	start := slices.Index(provs, first)
	for k := 1; k < len(provs); k++ {
		p := provs[(start+k)%len(provs)]
		for _, id := range r.Servers {
			if s, _ := c.Server(id); s.Provider == p {
				add(id, FallbackSameMetro)
			}
		}
	}
	for i := range out[1:] {
		out[i+1].Level = FallbackSameMetro
	}
	for _, id := range r.Nearby {
		add(id, FallbackNearby)
	}
	return out
}

// Catalog is every server and region.
type Catalog struct {
	Servers []Server // by provider file, then file order
	Regions []Region
	// Removed are the IDs of servers removed from the catalog.
	Removed []string
	byID    map[string]int
}

// IsRemoved reports a server removed from the catalog.
func (c *Catalog) IsRemoved(id string) bool { return slices.Contains(c.Removed, id) }

// Server returns the server with this ID.
func (c *Catalog) Server(id string) (Server, bool) {
	i, ok := c.byID[id]
	if !ok {
		return Server{}, false
	}
	return c.Servers[i], true
}

type providerFile struct {
	Provider string   `yaml:"provider"`
	Display  string   `yaml:"display"`
	Sources  []string `yaml:"sources"`
	Notes    string   `yaml:"notes"`
	Servers  []struct {
		ID       string         `yaml:"id"`
		Label    string         `yaml:"label"`
		Provider string         `yaml:"provider"`
		Host     string         `yaml:"host"`
		Ports    string         `yaml:"ports"`
		City     string         `yaml:"city"`
		Metro    string         `yaml:"metro"`
		Country  string         `yaml:"country"`
		Geo      string         `yaml:"geo"`
		Capacity float64        `yaml:"capacity_gbps"`
		Verified map[string]any `yaml:"verified"`
	} `yaml:"servers"`
}

type regionsFile struct {
	Regions []struct {
		ID      string   `yaml:"id"`
		Title   string   `yaml:"title"`
		Servers []string `yaml:"servers"`
		Nearby  []string `yaml:"nearby"`
		Others  []string `yaml:"others"`
		Default *bool    `yaml:"default"`
	} `yaml:"regions"`
	Removed []string `yaml:"removed"`
}

var (
	serverIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,40}$`)
	regionIDRe = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	countryRe  = regexp.MustCompile(`^[A-Z]{2}$`)
)

func decodeStrict(b []byte, v any) error {
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	return d.Decode(v)
}

// LoadCatalog reads every *.yml of fsys (regions.yml holds the regions)
// and reports every problem at once.
func LoadCatalog(fsys fs.FS) (*Catalog, error) {
	files, err := fs.Glob(fsys, "*.yml")
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	c := &Catalog{byID: map[string]int{}}
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	var regions []byte
	for _, f := range files {
		b, err := fs.ReadFile(fsys, f)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if f == "regions.yml" {
			regions = b
			continue
		}
		var pf providerFile
		if err := decodeStrict(b, &pf); err != nil {
			bad("%s: %v", f, err)
			continue
		}
		if pf.Display == "" || len(pf.Sources) == 0 || len(pf.Servers) == 0 {
			bad("%s: display, sources and servers are required", f)
		}
		for _, x := range pf.Servers {
			s := Server{ID: x.ID, Label: x.Label, Provider: x.Provider, Host: x.Host, City: x.City, Metro: x.Metro, Country: x.Country, Geo: x.Geo,
				CapacityMbps: x.Capacity * 1000}
			if s.Provider == "" {
				s.Provider = pf.Display
			}
			if s.Label == "" {
				s.Label = s.Provider + " " + s.City
			}
			lo, hi, ok := strings.Cut(x.Ports, "-")
			if !ok {
				hi = lo
			}
			var e1, e2 error
			s.PortMin, e1 = strconv.Atoi(lo)
			s.PortMax, e2 = strconv.Atoi(hi)
			switch {
			case !serverIDRe.MatchString(s.ID):
				bad("%s: server %q: id must be 1-40 characters of [A-Za-z0-9._-]", f, s.ID)
			case hasKey(c.byID, s.ID):
				bad("%s: server %q: duplicate id", f, s.ID)
			}
			if err := profile.CheckPublicHost(s.Host); err != nil {
				bad("%s: server %q: host: %v", f, s.ID, err)
			}
			if e1 != nil || e2 != nil || s.PortMin < 1 || s.PortMax > 65535 || s.PortMax < s.PortMin || s.ports() > 64 {
				bad("%s: server %q: ports %q, want first-last (at most 64)", f, s.ID, x.Ports)
			}
			if !countryRe.MatchString(s.Country) || s.City == "" || !regionIDRe.MatchString(s.Metro) {
				bad("%s: server %q: city, metro (lowercase) and an ISO country code are required", f, s.ID)
			}
			if x.Capacity < 0 {
				bad("%s: server %q: capacity_gbps %v", f, s.ID, x.Capacity)
			}
			if !slices.Contains(profile.Geos, s.Geo) {
				bad("%s: server %q: geo %q, want one of %s", f, s.ID, s.Geo, strings.Join(profile.Geos, ", "))
			}
			if _, ok := x.Verified["date"]; !ok {
				bad("%s: server %q: verified.date is required", f, s.ID)
			}
			c.byID[s.ID] = len(c.Servers)
			c.Servers = append(c.Servers, s)
		}
	}
	if regions != nil {
		var rf regionsFile
		if err := decodeStrict(regions, &rf); err != nil {
			bad("regions.yml: %v", err)
		}
		seen := map[string]bool{}
		for _, x := range rf.Regions {
			if !regionIDRe.MatchString(x.ID) || seen[x.ID] || x.Title == "" || len(x.Servers) == 0 {
				bad("regions.yml: region %q: a unique lowercase id, a title and servers are required", x.ID)
			}
			seen[x.ID] = true
			metro := ""
			for _, id := range x.Others {
				if _, ok := c.Server(id); !ok {
					bad("regions.yml: region %q: no server %q", x.ID, id)
				}
			}
			for i, id := range append(slices.Clone(x.Servers), x.Nearby...) {
				s, ok := c.Server(id)
				switch {
				case !ok:
					bad("regions.yml: region %q: no server %q", x.ID, id)
				case i < len(x.Servers) && metro != "" && s.Metro != metro:
					bad("regions.yml: region %q: %s is in metro %s, not %s (servers are one metro; use nearby)", x.ID, id, s.Metro, metro)
				case i == 0:
					metro = s.Metro
				}
			}
			c.Regions = append(c.Regions, Region{ID: x.ID, Title: x.Title, Servers: x.Servers, Nearby: x.Nearby, Others: x.Others, Default: x.Default == nil || *x.Default})
		}
		for _, id := range rf.Removed {
			switch {
			case !serverIDRe.MatchString(id) || slices.Contains(c.Removed, id):
				bad("regions.yml: removed %q: a unique server id", id)
			case hasKey(c.byID, id):
				bad("regions.yml: removed %q is still a server", id)
			}
			c.Removed = append(c.Removed, id)
		}
	}
	if len(c.Servers) == 0 {
		bad("no servers")
	}
	return c, errors.Join(errs...)
}

// Builtin returns the shipped catalog.
func Builtin() (*Catalog, error) { return LoadCatalog(profile.IperfCatalog()) }

// Custom parses --speedtest-server: a comma-separated list of catalog
// server IDs and host:port entries (one's own iperf3 server; private
// addresses are fine). Removed catalog servers are left out (the caller
// warns).
func Custom(list string, c *Catalog) ([]Server, error) {
	var out []Server
	removed := false
	for _, x := range strings.Split(list, ",") {
		x = strings.TrimSpace(x)
		if x == "" {
			continue
		}
		if c != nil {
			if s, ok := c.Server(x); ok {
				out = append(out, s)
				continue
			}
			if c.IsRemoved(x) {
				removed = true
				continue
			}
		}
		host, port, err := net.SplitHostPort(x)
		p, perr := strconv.Atoi(port)
		if err != nil || perr != nil || p < 1 || p > 65535 || host == "" || strings.ContainsAny(host, "/@ ") {
			return nil, fmt.Errorf("--speedtest-server %q: want host:port or a catalog server id (see the README)", x)
		}
		hp := net.JoinHostPort(host, port)
		out = append(out, Server{ID: hp, Label: "Your server", Provider: "Your server", Host: host, PortMin: p, PortMax: p, Custom: true, LAN: onLAN(host)})
	}
	if len(out) == 0 && removed {
		return nil, errors.New("--speedtest-server: every server listed was removed from the catalog")
	}
	if len(out) == 0 {
		return nil, errors.New("--speedtest-server: no servers")
	}
	return out, nil
}

// onLAN reports a host on the user's own network: a private IPv4 literal,
// or a name whose IPv4 addresses are all private (resolved once, at start;
// every test checks again).
func onLAN(host string) bool {
	if a, err := netip.ParseAddr(host); err == nil {
		return PrivateIPv4(a)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ips, err := lookupIPv4(ctx, host)
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, a := range ips {
		if !PrivateIPv4(a) {
			return false
		}
	}
	return true
}

// lookupIPv4 resolves A records (a variable: tests).
var lookupIPv4 = func(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
}

func hasKey[V any](m map[string]V, k string) bool {
	_, ok := m[k]
	return ok
}

// metroZones is each catalog metro's time zone: the finding asks whether a
// slowdown's hours are evening at the far end too.
var metroZones = map[string]string{
	"amsterdam": "Europe/Amsterdam", "atlanta": "America/New_York", "bay-area": "America/Los_Angeles", "chicago": "America/Chicago",
	"dallas": "America/Chicago", "frankfurt": "Europe/Berlin", "gravelines": "Europe/Paris", "hong-kong": "Asia/Hong_Kong",
	"kansas-city": "America/Chicago", "lille": "Europe/Paris", "london": "Europe/London", "los-angeles": "America/Los_Angeles",
	"manchester": "Europe/London", "miami": "America/New_York", "montreal": "America/Toronto", "new-york": "America/New_York",
	"paris": "Europe/Paris", "phoenix": "America/Phoenix", "portland": "America/Los_Angeles", "sao-paulo": "America/Sao_Paulo",
	"seattle": "America/Los_Angeles", "singapore": "Asia/Singapore", "strasbourg": "Europe/Paris", "sydney": "Australia/Sydney",
	"tashkent": "Asia/Tashkent", "tokyo": "Asia/Tokyo", "washington": "America/New_York",
}

// Zone is the server's local time zone (nil: unknown).
func (s Server) Zone() *time.Location {
	name, ok := metroZones[s.Metro]
	if !ok {
		return nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil
	}
	return loc
}

// Zone is the region's local time zone: its first server's.
func (r Region) Zone(c *Catalog) *time.Location {
	for _, id := range r.Servers {
		if s, ok := c.Server(id); ok {
			return s.Zone()
		}
	}
	return nil
}

// RegionOf is the one region a server belongs to (its ID; "" for none):
// a default region it is a server of, else one it backs up (nearby), else
// one it is listed under (others); then the same in opt-in regions.
func (c *Catalog) RegionOf(id string) string {
	for _, def := range []bool{true, false} {
		for _, list := range []func(Region) []string{
			func(r Region) []string { return r.Servers }, func(r Region) []string { return r.Nearby }, func(r Region) []string { return r.Others },
		} {
			for _, r := range c.Regions {
				if r.Default == def && slices.Contains(list(r), id) {
					return r.ID
				}
			}
		}
	}
	return ""
}

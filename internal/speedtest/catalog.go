package speedtest

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

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
//	    ports: 5201-5210              # one iperf3 instance per port, at most 64
//	    city: New York
//	    country: US                   # ISO 3166-1 alpha-2
//	    geo: na                       # na sa eu me af as oc
//	    verified: {date: 2026-10-10, tcp: true, iperf3: true}

// Server is one public iperf3 server: one iperf3 instance per port of
// PortMin..PortMax, each running one test at a time.
type Server struct {
	ID       string // identity of its stored results
	Label    string // public-safe: provider and city
	Provider string // display name
	Host     string // local only: never on the share link
	PortMin  int
	PortMax  int
	City     string
	Country  string
	Geo      string
	// Custom is a user-supplied server (--speedtest-server host:port): the
	// 15-minute floor does not apply when every server is one.
	Custom bool
}

func (s Server) ports() int { return s.PortMax - s.PortMin + 1 }

// Region is a region of mode "regions": its chain is tried in order.
type Region struct {
	ID    string
	Title string
	Chain []string // server IDs
}

// Catalog is every server and region.
type Catalog struct {
	Servers []Server // by provider file, then file order
	Regions []Region
	byID    map[string]int
}

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
		Country  string         `yaml:"country"`
		Geo      string         `yaml:"geo"`
		Verified map[string]any `yaml:"verified"`
	} `yaml:"servers"`
}

type regionsFile struct {
	Regions []struct {
		ID    string   `yaml:"id"`
		Title string   `yaml:"title"`
		Chain []string `yaml:"chain"`
	} `yaml:"regions"`
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
			s := Server{ID: x.ID, Label: x.Label, Provider: x.Provider, Host: x.Host, City: x.City, Country: x.Country, Geo: x.Geo}
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
			if !countryRe.MatchString(s.Country) || s.City == "" {
				bad("%s: server %q: city and an ISO country code are required", f, s.ID)
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
			if !regionIDRe.MatchString(x.ID) || seen[x.ID] || x.Title == "" || len(x.Chain) == 0 {
				bad("regions.yml: region %q: a unique lowercase id, a title and a chain are required", x.ID)
			}
			seen[x.ID] = true
			for _, id := range x.Chain {
				if _, ok := c.byID[id]; !ok {
					bad("regions.yml: region %q: no server %q", x.ID, id)
				}
			}
			c.Regions = append(c.Regions, Region{ID: x.ID, Title: x.Title, Chain: x.Chain})
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
// addresses are fine).
func Custom(list string, c *Catalog) ([]Server, error) {
	var out []Server
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
		}
		host, port, err := net.SplitHostPort(x)
		p, perr := strconv.Atoi(port)
		if err != nil || perr != nil || p < 1 || p > 65535 || host == "" || strings.ContainsAny(host, "/@ ") {
			return nil, fmt.Errorf("--speedtest-server %q: want host:port or a catalog server id (see the README)", x)
		}
		hp := net.JoinHostPort(host, port)
		out = append(out, Server{ID: hp, Label: "Your server", Provider: "Your server", Host: host, PortMin: p, PortMax: p, Custom: true})
	}
	if len(out) == 0 {
		return nil, errors.New("--speedtest-server: no servers")
	}
	return out, nil
}

func hasKey[V any](m map[string]V, k string) bool {
	_, ok := m[k]
	return ok
}

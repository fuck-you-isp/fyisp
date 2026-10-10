package speedtest

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/fuck-you-isp/fyisp/internal/profile"
)

// Server is one public iperf3 server: one iperf3 instance per port of
// PortMin..PortMax, each running one test at a time.
type Server struct {
	Name    string // identity of its stored results
	Host    string
	PortMin int
	PortMax int
	City    string
	Country string
	Geo     string
	// Custom is a user-supplied server (--speedtest-server): the only one
	// probed, and the 15-minute floor does not apply.
	Custom bool
}

func (s Server) ports() int { return s.PortMax - s.PortMin + 1 }

type serversFile struct {
	Sources []string `yaml:"sources"`
	Servers []struct {
		Name    string `yaml:"name"`
		Host    string `yaml:"host"`
		Ports   string `yaml:"ports"`
		City    string `yaml:"city"`
		Country string `yaml:"country"`
		Geo     string `yaml:"geo"`
	} `yaml:"servers"`
}

var (
	serverNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,40}$`)
	countryRe    = regexp.MustCompile(`^[A-Z]{2}$`)
)

// ParseServers reads a server list (catalog/iperf3/*.yml) and reports
// every problem at once.
func ParseServers(b []byte) ([]Server, error) {
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	var f serversFile
	if err := d.Decode(&f); err != nil {
		return nil, err
	}
	var out []Server
	var errs []error
	seen := map[string]bool{}
	for _, x := range f.Servers {
		s := Server{Name: x.Name, Host: x.Host, City: x.City, Country: x.Country, Geo: x.Geo}
		lo, hi, ok := strings.Cut(x.Ports, "-")
		if !ok {
			hi = lo
		}
		var err1, err2 error
		s.PortMin, err1 = strconv.Atoi(lo)
		s.PortMax, err2 = strconv.Atoi(hi)
		switch {
		case !serverNameRe.MatchString(s.Name):
			errs = append(errs, fmt.Errorf("server %q: name must be 1-40 characters of [A-Za-z0-9._-]", s.Name))
		case seen[s.Name]:
			errs = append(errs, fmt.Errorf("server %q: duplicate name", s.Name))
		}
		seen[s.Name] = true
		if err := profile.CheckPublicHost(s.Host); err != nil {
			errs = append(errs, fmt.Errorf("server %q: host: %v", s.Name, err))
		}
		if err1 != nil || err2 != nil || s.PortMin < 1 || s.PortMax > 65535 || s.PortMax < s.PortMin || s.ports() > 64 {
			errs = append(errs, fmt.Errorf("server %q: ports %q, want first-last (at most 64)", s.Name, x.Ports))
		}
		if !countryRe.MatchString(s.Country) {
			errs = append(errs, fmt.Errorf("server %q: country %q", s.Name, s.Country))
		}
		out = append(out, s)
	}
	if len(f.Sources) == 0 {
		errs = append(errs, errors.New("sources: a link to where the list comes from is required"))
	}
	if len(out) == 0 {
		errs = append(errs, errors.New("no servers"))
	}
	return out, errors.Join(errs...)
}

// Builtin returns the shipped server list (YABS's).
func Builtin() ([]Server, error) { return ParseServers(profile.IperfServersYAML()) }

// ParseCustom parses --speedtest-server host:port. Private addresses are
// allowed: an iperf3 server on the LAN or a VPS of one's own is the point.
func ParseCustom(hp string) (Server, error) {
	host, port, err := net.SplitHostPort(hp)
	if err != nil {
		return Server{}, fmt.Errorf("--speedtest-server %q: want host:port", hp)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 || host == "" || strings.ContainsAny(host, "/@ ") {
		return Server{}, fmt.Errorf("--speedtest-server %q: want host:port", hp)
	}
	return Server{Name: net.JoinHostPort(host, port), Host: host, PortMin: p, PortMax: p, Custom: true}, nil
}

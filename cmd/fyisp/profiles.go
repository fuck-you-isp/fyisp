package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/profile"
)

// profileNames is --profile: profile names, repeatable or comma-separated.
type profileNames []string

func (p *profileNames) String() string { return strings.Join(*p, ",") }

func (p *profileNames) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			*p = append(*p, s)
		}
	}
	return nil
}

// geoList is --geo: geo codes or region names, repeatable or comma-separated.
type geoList []string

func (g *geoList) String() string { return strings.Join(*g, ",") }

func (g *geoList) Set(v string) error {
	gs, err := profile.ParseGeos(v)
	if err != nil {
		return err
	}
	for _, x := range gs {
		if !slices.Contains(*g, x) {
			*g = append(*g, x)
		}
	}
	return nil
}

// profileCost is shown with target-limit errors: measured on the default
// schedule (HTTPS + TCP every 15s, ICMP every 5s per target).
const profileCost = "300 targets use about 4% of one CPU core and ~28 KB/s upload; cost grows linearly with the target count"

// loadProfile builds the profile of a run: --config, --profile, or the
// default.
func loadProfile(c config) (*model.Profile, error) {
	max := c.maxTargets
	if max == 0 {
		max = profile.DefaultMaxTargets
	}
	l := profile.Limits{MaxTargets: max}
	switch {
	case c.configPath != "":
		p, err := profile.LoadLimits(c.configPath, l)
		if err != nil {
			return nil, fmt.Errorf("loading --config: %w", err)
		}
		if c.noPath {
			profile.WithoutPath(p)
		}
		return p, nil
	case len(c.profiles) > 0 || len(c.geos) > 0:
		names := c.profiles
		if len(names) == 0 {
			names = []string{"default"}
		}
		r, err := profile.Builtin()
		if err != nil {
			return nil, err
		}
		p, err := r.Resolve(names, profile.ResolveOptions{NoPath: c.noPath, Geos: c.geos})
		if err != nil {
			return nil, fmt.Errorf("--profile: %w", err)
		}
		n := 0
		for _, t := range p.Targets {
			if t.Group != profile.PathGroup {
				n++
			}
		}
		if n == 0 {
			return nil, fmt.Errorf("--profile %s selects no targets (see `fyisp profiles`)", p.Name)
		}
		if len(p.Targets) > max {
			return nil, fmt.Errorf("--profile %s has %d targets, more than the limit of %d: pick narrower profiles or a region "+
				"(e.g. --geo eu; see `fyisp profiles`), or raise --max-targets (up to %d; %s)",
				p.Name, len(p.Targets), max, profile.MaxTargetsCap, profileCost)
		}
		if err := profile.Validate(p, l); err != nil {
			return nil, fmt.Errorf("--profile %s: %w", p.Name, err)
		}
		return p, nil
	}
	p, err := profile.Default()
	if err != nil {
		return nil, err
	}
	if c.noPath {
		profile.WithoutPath(p)
	}
	return p, nil
}

// cmdProfiles is `fyisp profiles [--geo eu] [name[,name...]]`: without
// names it lists the profiles, with names it prints their targets.
func cmdProfiles(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fyisp profiles", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var geos geoList
	fs.Var(&geos, "geo", "only targets in these regions: na, sa, eu, me, af, as, oc (or north-america, europe, ...)")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: fyisp profiles                 list the target profiles\n       fyisp profiles [--geo eu] NAME[,NAME...]   print their targets\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	r, err := profile.Builtin()
	if err != nil {
		fmt.Fprintln(stderr, "fyisp:", err)
		return 1
	}
	var names profileNames
	for _, a := range fs.Args() {
		_ = names.Set(a)
	}
	if len(names) == 0 {
		return listProfiles(r, geos, stdout, stderr)
	}
	return showProfile(r, names, geos, stdout, stderr)
}

func listProfiles(r *profile.Registry, geos geoList, stdout, stderr io.Writer) int {
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	row := func(d *profile.Definition) {
		p, err := r.Resolve([]string{d.Name}, profile.ResolveOptions{NoPath: true, Geos: geos})
		if err != nil {
			fmt.Fprintf(w, "%s\t-\t-\t%s (error: %v)\n", d.Name, d.Description, err)
			return
		}
		mark := ""
		if len(p.Targets)+5 > profile.DefaultMaxTargets {
			mark = "*"
		}
		fmt.Fprintf(w, "%s\t%d%s\t%d\t%s\n", d.Name, len(p.Targets), mark, len(p.Groups), d.Description)
	}
	where := ""
	if len(geos) > 0 {
		where = " (only --geo " + geos.String() + ")"
	}
	fmt.Fprintf(w, "PROFILE\tTARGETS%s\tPANELS\tDESCRIPTION\n", where)
	var providers []*profile.Definition
	for _, d := range r.Definitions() {
		if d.Provider != "" {
			providers = append(providers, d)
			continue
		}
		row(d)
	}
	if len(providers) > 0 {
		fmt.Fprintf(w, "\nPROVIDER\tTARGETS\tPANELS\tDESCRIPTION\n")
		for _, d := range providers {
			row(d)
		}
	}
	if err := w.Flush(); err != nil {
		fmt.Fprintln(stderr, "fyisp:", err)
		return 1
	}
	fmt.Fprintf(stdout, `
Run with:  fyisp --profile NAME[,NAME...]   (a union; --geo eu keeps only one region)
Details:   fyisp profiles NAME[,NAME...]
Every run also measures the 5-target "Network path" group (off with --no-path).
* over the default limit of %d targets (with the network path): add --geo or
  --max-targets N (up to %d). %s.
`, profile.DefaultMaxTargets, profile.MaxTargetsCap, profileCost)
	return 0
}

func showProfile(r *profile.Registry, names profileNames, geos geoList, stdout, stderr io.Writer) int {
	p, err := r.Resolve(names, profile.ResolveOptions{NoPath: true, Geos: geos})
	if err != nil {
		fmt.Fprintln(stderr, "fyisp:", err)
		return 1
	}
	fmt.Fprintf(stdout, "Profile %s: %d targets in %d panels (plus the 5-target Network path group)\n", p.Name, len(p.Targets), len(p.Groups))
	if len(p.Targets)+5 > profile.DefaultMaxTargets {
		fmt.Fprintf(stdout, "Over the default limit of %d: run it with --max-targets %d (up to %d).\n", profile.DefaultMaxTargets, len(p.Targets)+5, profile.MaxTargetsCap)
	}
	for _, g := range p.Groups {
		fmt.Fprintf(stdout, "\n%s  [%s]\n", g.Title, g.ID)
		w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		for _, t := range p.Targets {
			if t.Group != g.ID {
				continue
			}
			var kinds []string
			for _, k := range t.Kinds {
				kinds = append(kinds, k.String())
			}
			where := ""
			if ct, ok := r.Catalog.Target(t.Name); ok && ct.Host == t.Host {
				where = strings.Join(nonEmpty(ct.Region, ct.City, ct.Country, geoOf(ct)), " ")
			}
			target := t.Host
			if t.Port != 0 && t.Port != 443 {
				target += fmt.Sprintf(":%d", t.Port)
			}
			if t.Path != "" && t.Path != "/" {
				target += t.Path
			}
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", t.Name, target, strings.Join(kinds, ","), where)
		}
		_ = w.Flush()
	}
	if sk := r.SkippedIn(names); len(sk) > 0 {
		fmt.Fprintf(stdout, "\nSkipped (unreachable when last checked, kinds: []):\n")
		w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		for _, t := range sk {
			if len(geos) > 0 && !slices.Contains(geos, t.GeoKey()) {
				continue
			}
			fmt.Fprintf(w, "  %s\t%s\tunreachable, skipped\n", t.Name, t.Host)
		}
		_ = w.Flush()
	}
	return 0
}

func geoOf(t profile.CatalogTarget) string {
	if t.Anycast {
		return "(anycast)"
	}
	return profile.GeoTitles[t.Geo]
}

func nonEmpty(ss ...string) []string {
	var out []string
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// runProfiles is the entry point from main.
func runProfiles() { os.Exit(cmdProfiles(os.Args[2:], os.Stdout, os.Stderr)) }

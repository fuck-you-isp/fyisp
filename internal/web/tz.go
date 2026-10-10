package web

import (
	"math"
	"regexp"
	"strconv"
	"sync"
	"time"
	_ "time/tzdata" // IANA zones on every platform (the image is FROM scratch)
)

// zoneParam is a tz= parameter: an IANA zone name (e.g. Europe/Berlin, the
// browser's Intl.DateTimeFormat().resolvedOptions().timeZone), right on
// both sides of a DST change; or the older minutes east of UTC (a fixed
// offset, rounded to 15 min). Empty: the server's local zone.
type zoneParam struct {
	loc  *time.Location
	name string // the IANA name; "" for minutes or the local zone
	min  int    // minutes east of UTC now (the minutes form: as given)
	key  string // for cache keys
}

var (
	zoneName  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+\-]*(/[A-Za-z0-9_+\-]+){0,2}$`)
	zoneMu    sync.Mutex
	zoneCache = map[string]*time.Location{}
)

// parseZone reads tz= (see zoneParam).
func parseZone(s string, now time.Time) (zoneParam, error) {
	if s == "" {
		_, off := now.In(time.Local).Zone()
		return zoneParam{loc: time.Local, min: off / 60, key: "local"}, nil
	}
	if v, err := strconv.Atoi(s); err == nil {
		if v < -840 || v > 840 {
			return zoneParam{}, badReq("bad tz %q: want an IANA zone such as Europe/Berlin, or minutes east of UTC, -840 to 840", s)
		}
		m := int(math.Round(float64(v)/15)) * 15
		return zoneParam{loc: time.FixedZone("", m*60), min: m, key: strconv.Itoa(m)}, nil
	}
	if len(s) > 64 || !zoneName.MatchString(s) {
		return zoneParam{}, badReq("bad tz %q: want an IANA zone such as Europe/Berlin, or minutes east of UTC", s)
	}
	zoneMu.Lock()
	loc, ok := zoneCache[s]
	zoneMu.Unlock()
	if !ok {
		var err error
		if loc, err = time.LoadLocation(s); err != nil || s == "Local" {
			return zoneParam{}, badReq("unknown tz %q: want an IANA zone such as Europe/Berlin", s)
		}
		zoneMu.Lock()
		if len(zoneCache) < 512 {
			zoneCache[s] = loc
		}
		zoneMu.Unlock()
	}
	_, off := now.In(loc).Zone()
	return zoneParam{loc: loc, name: s, min: off / 60, key: s}, nil
}

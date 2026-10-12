package profile

import (
	"fmt"
	"slices"
	"strings"

	"github.com/fuck-you-isp/fyisp/internal/callq"
	"github.com/fuck-you-isp/fyisp/internal/model"
)

// Home categories: the Overview's filter (the v0.6 Home's tiles). A target
// names its category with a tag "home:<category>" (the default's targets
// all do); targets without one get a derived category (see HomeCategory).
const (
	TagHome   = "home:"
	HomeCalls = "calls" // the call path and the call and chat apps
	HomeWork  = "work"  // work apps (Microsoft 365, Google Workspace, ...)
	HomeWeb   = "web"   // websites: CDNs and streaming
	HomeOther = "other" // everything else; never a tag
)

// HomeCategories are the values a home: tag may have, in tile order.
var HomeCategories = []string{HomeCalls, HomeWork, HomeWeb}

// HomeCategory returns the Home category of a target: HomeCalls, HomeWork,
// HomeWeb or HomeOther. A home: tag wins; otherwise an app:* or udp-path
// tag means calls, the productivity provider work, a CDN or the streaming
// provider web, and anything else other.
func HomeCategory(t model.Target) string {
	for _, tag := range t.Tags {
		if c, ok := strings.CutPrefix(tag, TagHome); ok && slices.Contains(HomeCategories, c) {
			return c
		}
	}
	switch {
	case callq.IsApp(t) || slices.Contains(t.Tags, callq.TagUDPPath):
		return HomeCalls
	case t.Provider == "services-productivity":
		return HomeWork
	case t.ProviderKind == "cdn" || t.Provider == "services-streaming":
		return HomeWeb
	}
	return HomeOther
}

// checkHomeTags reports an unknown home: value, or more than one home: tag.
func checkHomeTags(tags []string) error {
	n := 0
	for _, tag := range tags {
		c, ok := strings.CutPrefix(tag, TagHome)
		if !ok {
			continue
		}
		if !slices.Contains(HomeCategories, c) {
			return fmt.Errorf("tag %q: want %s%s", tag, TagHome, strings.Join(HomeCategories, "|"+TagHome))
		}
		if n++; n > 1 {
			return fmt.Errorf("more than one %s tag", TagHome)
		}
	}
	return nil
}

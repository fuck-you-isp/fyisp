// Package profile loads target profiles: the embedded default and local
// --config files (which may extend the default).
package profile

import "github.com/fuck-you-isp/fyisp/internal/model"

// Limits bound what a profile may ask for.
type Limits struct {
	MaxTargets      int  // default 300
	MinIntervalSecs int  // default 5
	AllowPrivateIPs bool // true only for local files
}

// Interface summary (implemented by the profile agent):
//
//	func Default() (*model.Profile, error)          // embedded default.yml: 87 targets, 7 groups
//	func Load(path string) (*model.Profile, error)  // --config; `extends: [default]`, add/remove/override by name
//	func Validate(p *model.Profile, l Limits) error // unique names, limits, groups exist
var _ = model.Profile{}

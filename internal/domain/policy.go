package domain

// TypePolicy is the operator's rule for one capability (roadmap item 7).
type TypePolicy struct {
	Enabled bool `json:"enabled"`
	// NodeLabels restricts the capability to nodes carrying all of these
	// operator labels (fleet.go) — e.g. raw commands only on the laptop
	// labelled raw=ok.
	NodeLabels map[string]string `json:"nodeLabels,omitempty"`
	// MaxRuntimeSeconds bounds each attempt (Workload.TimeoutSeconds);
	// 0 = no limit.
	MaxRuntimeSeconds int `json:"maxRuntimeSeconds,omitempty"`
}

// Policy decides what the fleet may be asked to run. A catalog task type
// without an entry is enabled; raw capabilities (system.execute,
// filesystem.read) and any capability outside the catalog without an
// entry follow AllowUnlisted.
type Policy struct {
	Types         map[CapabilityName]TypePolicy `json:"types,omitempty"`
	AllowUnlisted bool                          `json:"allowUnlisted,omitempty"`
	// SpotCheckPercent is the share (0-100) of each job's deterministic
	// tasks the manager re-runs on a different device to compare results,
	// at least one per job; 0 = off (spotcheck.go). Only worth it once
	// devices you don't control join.
	SpotCheckPercent int `json:"spotCheckPercent,omitempty"`
}

// PermissivePolicy allows everything (embedding and tests, and the
// behavior before policy existed).
func PermissivePolicy() Policy { return Policy{AllowUnlisted: true} }

// DefaultPolicy is what a new manager starts with: the sandboxed catalog
// types enabled, raw commands and host file reads off until the operator
// opts in.
func DefaultPolicy() Policy {
	return Policy{Types: map[CapabilityName]TypePolicy{
		CapabilitySystemExecute:  {Enabled: false},
		CapabilityFilesystemRead: {Enabled: false},
	}}
}

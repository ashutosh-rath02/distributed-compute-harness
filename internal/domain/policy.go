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

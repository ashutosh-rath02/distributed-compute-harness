package domain

// FeatureInputCache means the agent keeps the input files it downloaded
// in a bounded cache, copies a cached one into a workload's working
// directory (checking its size and SHA-256 again, as for a download)
// instead of downloading it, and tells the manager which files it holds
// (InputCacheReport). The manager then prefers it for work on those files,
// but only between devices that are equally busy. Agents without it get
// no such preference: they behave exactly as before.
const FeatureInputCache = "input-cache.v1"

// InputCacheReport says which input files an agent holds: the first
// InputCachePrefixLen hex characters of each one's SHA-256, most recently
// used first, at most MaxInputCacheReport of them. It is an untrusted
// hint: it may only make a device preferred, never decide what runs or
// with which bytes.
type InputCacheReport struct {
	Prefixes []string `json:"prefixes"`
}

// A 64-bit prefix keeps the report small while making an accidental
// match between two different files practically impossible (and a match
// would only cost a preference).
const (
	InputCachePrefixLen = 16
	MaxInputCacheReport = 128
)

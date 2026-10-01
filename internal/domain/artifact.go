package domain

import (
	"fmt"
	"strings"
)

// ArtifactRef names one file a workload reads or produces: Name is its
// path inside the workload's working directory, SHA256 (lowercase hex) and
// Size identify the exact bytes in the manager's content-addressed store.
type ArtifactRef struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// FeatureArtifacts means the agent fetches a workload's Inputs into a
// working directory before running it and uploads its declared Outputs
// afterwards. Placement only sends workloads with files to agents that
// advertise it: an older agent would ignore both lists and run the
// command without its files.
const FeatureArtifacts = "artifacts.v1"

// Per-workload limits on declared files.
const (
	MaxWorkloadInputs  = 16
	MaxWorkloadOutputs = 16
)

// ValidSHA256 reports whether s is a lowercase hex SHA-256 digest — the
// only form an artifact ID may take before it is used to build a path.
func ValidSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// ValidWorkloadID reports whether id has the form the manager generates
// (32 lowercase hex characters) — required before an agent uses it to
// name a working directory.
func ValidWorkloadID(id WorkloadID) bool {
	if len(id) != 32 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// windowsDeviceNames can't be used as a file name on Windows, with or
// without an extension ("nul.txt" opens the NUL device).
var windowsDeviceNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true, "com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true, "lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// ValidArtifactName checks a workload file name: a relative,
// forward-slash path of at most 4 segments and 128 bytes, each segment
// [A-Za-z0-9._-]{1,64}, never "." or "..", never ending in a dot (Windows
// strips it), never a Windows device name. It is the same on every
// platform, so a name valid for one agent is valid for all and can't
// escape the working directory on any of them.
func ValidArtifactName(name string) error {
	if name == "" || len(name) > 128 {
		return fmt.Errorf("file name %q must be 1-128 bytes", name)
	}
	segments := strings.Split(name, "/")
	if len(segments) > 4 {
		return fmt.Errorf("file name %q has more than 4 path segments", name)
	}
	for _, seg := range segments {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("file name %q must be a relative path without empty, \".\" or \"..\" segments", name)
		}
		if len(seg) > 64 {
			return fmt.Errorf("file name %q has a segment longer than 64 bytes", name)
		}
		for i := 0; i < len(seg); i++ {
			c := seg[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
				return fmt.Errorf("file name %q may only use letters, digits, '.', '_', '-' and '/'", name)
			}
		}
		if strings.HasSuffix(seg, ".") {
			return fmt.Errorf("file name %q has a segment ending in '.'", name)
		}
		base := strings.ToLower(seg)
		if i := strings.IndexByte(base, '.'); i >= 0 {
			base = base[:i]
		}
		if windowsDeviceNames[base] {
			return fmt.Errorf("file name %q uses a reserved Windows device name", name)
		}
	}
	return nil
}

// ValidateWorkloadFiles checks a workload's input and output names
// together: each valid, at most the per-workload limits, and no two that
// would land on the same file or directory — compared case-insensitively
// (Windows and macOS filesystems are), and no name may be a path prefix of
// another ("a" and "a/b" can't both exist).
func ValidateWorkloadFiles(inputs []ArtifactRef, outputs []string) error {
	if len(inputs) > MaxWorkloadInputs {
		return fmt.Errorf("at most %d input files", MaxWorkloadInputs)
	}
	if len(outputs) > MaxWorkloadOutputs {
		return fmt.Errorf("at most %d output files", MaxWorkloadOutputs)
	}
	names := make([]string, 0, len(inputs)+len(outputs))
	for _, in := range inputs {
		if !ValidSHA256(in.SHA256) {
			return fmt.Errorf("input %q: sha256 must be 64 lowercase hex characters", in.Name)
		}
		names = append(names, in.Name)
	}
	names = append(names, outputs...)
	folded := make([]string, len(names))
	for i, n := range names {
		if err := ValidArtifactName(n); err != nil {
			return err
		}
		folded[i] = strings.ToLower(n)
	}
	for i := range folded {
		for j := range folded {
			if i == j {
				continue
			}
			if folded[i] == folded[j] {
				return fmt.Errorf("file names %q and %q name the same file", names[i], names[j])
			}
			if strings.HasPrefix(folded[j], folded[i]+"/") {
				return fmt.Errorf("file name %q is a directory of %q; a name can't be both", names[i], names[j])
			}
		}
	}
	return nil
}

// HasFiles reports whether w declares any input or output files.
func (w Workload) HasFiles() bool { return len(w.Inputs) > 0 || len(w.Outputs) > 0 }

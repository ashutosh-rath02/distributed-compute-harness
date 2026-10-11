// Package version holds the build's identity: a semantic version, the
// commit and the build date. The release and build scripts set them with
// -ldflags; a plain `go build` reports a development build.
//
//	go build -ldflags "-X home-harness/internal/version.Version=1.2.3 \
//	  -X home-harness/internal/version.Commit=abc1234 \
//	  -X home-harness/internal/version.Date=2026-10-09T12:00:00Z" ./cmd/agent
//
// Updates never compare these strings: self-update decides by binary
// hash, so the version is for people (and for telling builds apart in
// GET /nodes), not for protocol decisions.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Set by -ldflags -X; they must stay plain string variables.
var (
	// Version is the semantic version, without a leading "v".
	Version = "0.0.0-dev"
	// Commit is the git commit the build came from.
	Commit = ""
	// Date is the build (or commit) time, RFC 3339.
	Date = ""
)

// Info is what -version flags and GET /version report. It carries
// nothing secret.
type Info struct {
	Version  string `json:"version"`
	Commit   string `json:"commit,omitempty"`
	Date     string `json:"date,omitempty"`
	Platform string `json:"platform"`
	Go       string `json:"go"`
}

// Get returns this binary's identity. When the build did not inject a
// commit, it falls back to the VCS stamp Go embeds, so even a plain
// `go build` of a checkout says which commit it is.
func Get() Info {
	commit := Commit
	if commit == "" {
		commit = vcsRevision()
	}
	return Info{
		Version:  Version,
		Commit:   commit,
		Date:     Date,
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
		Go:       runtime.Version(),
	}
}

// String is the one-line form for -version: "name 1.2.3 (commit abc1234,
// built 2026-10-09T12:00:00Z, linux/arm64, go1.27.0)".
func String(name string) string {
	i := Get()
	s := name + " " + i.Version
	extra := ""
	if i.Commit != "" {
		extra += "commit " + short(i.Commit)
	}
	if i.Date != "" {
		if extra != "" {
			extra += ", "
		}
		extra += "built " + i.Date
	}
	if extra != "" {
		extra += ", "
	}
	return fmt.Sprintf("%s (%s%s, %s)", s, extra, i.Platform, i.Go)
}

func short(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}

func vcsRevision() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	rev, dirty := "", false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev != "" && dirty {
		rev += "-dirty"
	}
	return rev
}

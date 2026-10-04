//go:build !windows

package keepawake

// Elsewhere there is no request to hold yet (macOS could run
// "caffeinate -i"; Linux desktops have systemd-inhibit). Holding is a
// no-op, so callers need no platform checks.

const platformSupported = false

type platformRequest struct{}

func newPlatformRequest(string) platformRequest { return platformRequest{} }

func (platformRequest) set(bool) error { return nil }

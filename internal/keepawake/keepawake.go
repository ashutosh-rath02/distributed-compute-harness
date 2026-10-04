// Package keepawake asks the operating system not to sleep while the
// harness has work in flight: a manager PC that sleeps takes the whole
// fleet down, and a worker that sleeps loses its task. It never blocks a
// sleep the user asks for (closing the lid, Start > Sleep), only the idle
// timeout, and it is released as soon as the work is done.
package keepawake

import "sync"

// Request is one keep-awake request, held or not. The zero value is not
// usable; use New. Safe for concurrent use.
type Request struct {
	mu   sync.Mutex
	held bool
	sys  platformRequest
}

// New prepares a request whose reason the OS shows (Windows: powercfg
// /requests). Where the platform has no such request it does nothing.
func New(reason string) *Request {
	return &Request{sys: newPlatformRequest(reason)}
}

// Hold holds the request (on) or releases it, if that changes anything.
// It reports whether the OS accepted the change.
func (r *Request) Hold(on bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if on == r.held {
		return nil
	}
	if err := r.sys.set(on); err != nil {
		return err
	}
	r.held = on
	return nil
}

// Held reports whether the request is held.
func (r *Request) Held() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.held
}

// Supported reports whether holding the request has any effect here.
func Supported() bool { return platformSupported }

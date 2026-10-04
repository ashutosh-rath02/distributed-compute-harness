package keepawake

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows: a power request object (PowerCreateRequest), set to "system
// required" and "execution required" while held. Unlike
// SetThreadExecutionState it belongs to no thread, so any goroutine may
// set or clear it. It shows in powercfg /requests with its reason.

const platformSupported = true

var (
	kernel32               = windows.NewLazySystemDLL("kernel32.dll")
	procPowerCreateRequest = kernel32.NewProc("PowerCreateRequest")
	procPowerSetRequest    = kernel32.NewProc("PowerSetRequest")
	procPowerClearRequest  = kernel32.NewProc("PowerClearRequest")
)

const (
	powerRequestContextSimpleString = 0x1
	powerRequestSystemRequired      = 1
	powerRequestExecutionRequired   = 3
)

// reasonContext is REASON_CONTEXT with the simple-string form of its
// union in the first word (the union is three words on 64-bit Windows).
type reasonContext struct {
	Version uint32
	Flags   uint32
	Reason  [3]uintptr
}

type platformRequest struct {
	handle windows.Handle
	err    error
}

func newPlatformRequest(reason string) platformRequest {
	text, err := windows.UTF16PtrFromString(reason)
	if err != nil {
		return platformRequest{err: err}
	}
	ctx := reasonContext{Flags: powerRequestContextSimpleString}
	ctx.Reason[0] = uintptr(unsafe.Pointer(text))
	h, _, callErr := procPowerCreateRequest.Call(uintptr(unsafe.Pointer(&ctx)))
	if windows.Handle(h) == windows.InvalidHandle || h == 0 {
		return platformRequest{err: fmt.Errorf("keepawake: PowerCreateRequest: %w", callErr)}
	}
	return platformRequest{handle: windows.Handle(h)}
}

func (p platformRequest) set(on bool) error {
	if p.err != nil {
		return p.err
	}
	proc := procPowerClearRequest
	if on {
		proc = procPowerSetRequest
	}
	for _, kind := range []uintptr{powerRequestSystemRequired, powerRequestExecutionRequired} {
		if ok, _, err := proc.Call(uintptr(p.handle), kind); ok == 0 {
			return fmt.Errorf("keepawake: %s(%d): %w", proc.Name, kind, err)
		}
	}
	return nil
}

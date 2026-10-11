package agent

import (
	"context"
	"time"
)

// transferTries is how many times a workload file is fetched or uploaded
// before the attempt fails. A link that drops mid-transfer (wifi roaming,
// a relay restarting) is the usual cause, and the next try normally just
// works; failing the whole attempt instead throws away the slot, the
// queue position and, for a job, one of its attempts.
const transferTries = 3

// transferRetryWait is the pause before try n+1; a variable so tests are fast.
var transferRetryWait = func(n int) time.Duration { return time.Duration(n) * 300 * time.Millisecond }

// retryTransfer runs do up to transferTries times, stopping early when ctx
// ends (the workload was canceled or timed out). Fetch verifies size and
// SHA-256 before the file appears and Upload is re-sent whole, so a retry
// can never leave a half-written file behind.
func retryTransfer(ctx context.Context, do func() error) error {
	var err error
	for n := 1; n <= transferTries; n++ {
		if err = do(); err == nil || ctx.Err() != nil {
			return err
		}
		if n < transferTries {
			select {
			case <-ctx.Done():
				return err
			case <-time.After(transferRetryWait(n)):
			}
		}
	}
	return err
}

package main

import (
	"errors"
	"net"
)

// isConnectionRefused reports whether err is a failure to connect at all
// (nothing listening, no route), as opposed to a manager that answered: the
// first-run mistake of running harnessctl where no manager is.
func isConnectionRefused(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

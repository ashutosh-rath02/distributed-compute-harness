package agent

import (
	"errors"
	"log"

	"home-harness/internal/protocol"
)

// Pairing (Config.Pairing, cmd/agent -pair): a device joins by the
// operator's approval instead of a token. Its first REGISTER makes it a
// join request on the manager; until someone approves it there, every
// REGISTER is answered "pending" with the manager's pairing code, and the
// agent asks again every PairingRetry. Both sides compute the code
// themselves (protocol.PairingCode) from the manager's certificate and
// this device's key, so the operator approving it can check that the two
// screens agree.

// PendingApprovalError is a REGISTER the manager is holding as a join
// request. Code is this device's own pairing code; ManagerCode the one the
// manager reported (different only if something sits between the two).
type PendingApprovalError struct {
	Code, ManagerCode string
}

func (e *PendingApprovalError) Error() string {
	return "waiting for approval on the manager, pairing code " + e.Code
}

// PairingCode is the code this device shows while waiting for approval:
// from the manager certificate it pins (or saw first) and its own key.
func (a *Agent) PairingCode() string {
	return protocol.PairingCode(a.managerFingerprint(), a.identity.PublicKey)
}

func (a *Agent) managerFingerprint() string {
	if a.cfg.ManagerFingerprintFunc != nil {
		return a.cfg.ManagerFingerprintFunc()
	}
	return a.cfg.ManagerFingerprint
}

// notePending logs a pending answer the first time its code is seen (the
// agent asks again every couple of seconds; the log would fill otherwise)
// and warns loudly when the manager's code differs from this device's.
func (a *Agent) notePending(e *PendingApprovalError) {
	a.pairMu.Lock()
	defer a.pairMu.Unlock()
	key := e.Code + "/" + e.ManagerCode
	if a.loggedPending == key {
		return
	}
	a.loggedPending = key
	if e.ManagerCode != "" && e.ManagerCode != e.Code {
		log.Printf("agent %s: WARNING: the manager shows pairing code %s but this device computed %s: something is between them. Do NOT approve it.", a.identity.NodeID, e.ManagerCode, e.Code)
		return
	}
	log.Printf("agent %s: pairing: approve this device on the manager, code %s", a.identity.NodeID, e.Code)
}

func isPendingApproval(err error) bool {
	var p *PendingApprovalError
	return errors.As(err, &p)
}

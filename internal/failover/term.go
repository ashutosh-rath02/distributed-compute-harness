// Package failover is the standby manager (roadmap item 17): a second
// manager that copies the active one's whole state — database, TLS
// identity, pairing token, AI key and stored files — over the
// authenticated, pinned standby link, does not serve agents while it is a
// standby, and takes over when promoted.
//
// The fencing rule, which is what keeps two managers from serving at
// once:
//
//  1. Every manager state carries a failover term in its database (0 for
//     a manager that never failed over); a standby's copy carries the
//     primary's.
//  2. A promotion sets term = max(the copied term, the highest term the
//     standby has otherwise seen) + 1 and writes it to the database
//     (fsynced) before the promoted manager accepts a single agent. A
//     manual promotion is refused while the primary still answers, unless
//     forced (a planned switch-over).
//  3. An active manager steps down — refuses registrations, drops its
//     agents, stops dispatching, and becomes a standby of its peer — as
//     soon as it learns of a term higher than its own, from either:
//     a. an agent's REGISTER carrying that term with a valid proof: a
//     signature over it by the manager key the pair shares (so a
//     hostile agent cannot invent one), or
//     b. its peer's /standby/status, over TLS pinned to that shared
//     identity and authenticated with the pair secret, reporting it is
//     active at that term. A manager asks this at every start, before
//     it listens for agents, and every 10 s while active.
//  4. A failover.v1 agent accepts a manager only if the term in its
//     REGISTER_ACK is at least the highest it has seen for that manager
//     identity, and reports that term (with its proof) whenever it
//     registers.
//
// Limits: one standby per manager (two standbys could each promote to
// the same term). Under a network partition, agents that only know the
// primary (older agents, or new ones that never saw the higher term) can
// keep using an old primary until a fence above fires; whatever that
// manager did meanwhile is discarded when it steps down (its database is
// replaced by the active one's). Work running at the moment of failover
// is treated exactly like after a manager restart: UNKNOWN, and batch
// jobs retry it. Relay-connected agents and -insecure managers are out of
// scope: the standby link needs pinned TLS on the LAN.
package failover

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"strconv"
)

// termContext prefixes what a term proof signs, so it can never be taken
// for any other signature made with the manager key.
const termContext = "home-harness-failover-term-v1:"

func termDigest(term uint64) []byte {
	sum := sha256.Sum256([]byte(termContext + strconv.FormatUint(term, 10)))
	return sum[:]
}

// SignTerm proves term was issued by a holder of the manager key (only an
// active manager of this pair signs its own term).
func SignTerm(cert tls.Certificate, term uint64) ([]byte, error) {
	signer, ok := cert.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, errors.New("failover: the manager key cannot sign")
	}
	return signer.Sign(rand.Reader, termDigest(term), crypto.SHA256)
}

// VerifyTerm reports whether proof is a valid SignTerm(term) by cert's key.
func VerifyTerm(cert tls.Certificate, term uint64, proof []byte) bool {
	if len(cert.Certificate) == 0 || len(proof) == 0 || len(proof) > 512 {
		return false
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return false
	}
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return false
	}
	return ecdsa.VerifyASN1(pub, termDigest(term), proof)
}

// Package remoteaccess is the wire protocol of the remote dashboard: the
// owner's browser (or harnessctl -remote) using the manager's operator API
// from anywhere, through the relay (internal/relay's public gateway), with
// a credential the relay never sees and requests it can neither read,
// replay nor change.
//
// # Why not end-to-end TLS through the relay
//
// The obvious design is TLS passthrough: the relay peeks at the browser's
// ClientHello, maps the hostname to a manager and splices raw bytes, so
// TLS ends at the manager. The relay could do that (an SNI router next to
// its rendezvous port, plus wildcard DNS), but it does not stop the relay
// operator, the party this protects against:
//   - the manager's certificate is self-signed, so every browser shows a
//     warning; a relay presenting its own self-signed certificate gets the
//     identical warning, and nobody compares fingerprints by hand;
//   - a browser-trusted certificate for a per-manager name does not help
//     either: whoever controls that name's DNS or IP (the relay operator)
//     can have a certificate issued for it too.
//
// So the relay keeps terminating the browser's TLS with its own trusted
// certificate (as it already does for invitations), and the protection
// lives one layer up, in this package.
//
// # The design
//
// The manager generates the remote key: 24 symbols of Crockford base32,
// 120 bits. Never a passphrase the owner picks: the relay sees every
// login proof, and could guess a human-chosen passphrase offline from one
// of them. The key is separate from the operator token and opens only
// what the remote mode allows (internal/manager/remote.go).
//
//  1. Challenge: POST hello returns a fresh server nonce (single use,
//     two minutes).
//  2. Login: the client proves it holds the key with
//     HMAC(K_login, "client sn cn") over the server nonce sn and its own
//     nonce cn; the manager answers HMAC(K_login, "server sn cn"), so the
//     page knows it reached the real manager. K_login and the two session
//     keys are HKDF-SHA256 outputs of the key, the session keys bound to
//     both nonces: a recorded login is useless (the nonce is spent) and
//     tells the relay nothing it can guess the key from.
//  3. Every call afterwards is one AES-256-GCM message under the session's
//     client-to-manager key: method, path and body inside, the session id
//     in the additional data, a per-session sequence number in the nonce.
//     The manager accepts each sequence number once (a sliding window, so
//     the dashboard's parallel requests may arrive out of order). The
//     answer streams back as sealed frames under the other key, numbered
//     in the nonce and ended by an explicit end frame, so a reordered,
//     dropped, altered or cut-short reply fails to open.
//
// In the browser (remote.js) the key is imported as a non-extractable
// WebCrypto key the moment it is typed, and every key derived from it is
// non-extractable too; "remember on this device" stores that key object in
// IndexedDB, never its bytes.
//
// # What a malicious relay can and cannot do
//
// It can: refuse or delay anything (deny service); see that remote access
// is used, when, and how much; and, because the page's code itself comes
// through the relay, serve a modified page. A modified page can capture
// the key when the owner types it in, or, with a remembered key, act
// through the owner's open page; either way only within the remote mode
// (sensitive actions need full control, and secrets such as the pairing
// token are never served remotely), and the owner cuts it off with a new
// key (harnessctl remote rotate) or by turning remote access off. Using
// harnessctl -remote instead of the page avoids that: its code is not
// served by the relay.
//
// It cannot: learn the key or any session key from the traffic; read
// what is asked or answered (all of it is sealed); replay, reorder or
// alter a request (it fails to open, or its sequence number was spent);
// forge or alter an answer the page shows; or change the remote settings
// (never available remotely).
package remoteaccess

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Alphabet is Crockford's base32: no I, L, O or U, so a key read aloud or
// typed on a phone is hard to get wrong. KeySymbols of it make a key.
const (
	Alphabet   = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	KeySymbols = 24
)

// Limits shared by both ends.
const (
	// MaxBody bounds a request's body (sealed in one message, so it is
	// held in memory on both ends): enough for a photo to process, not a
	// way to fill the manager's memory through the relay.
	MaxBody = 8 << 20
	// MaxSealedRequest is MaxBody plus the request head and the GCM tag.
	MaxSealedRequest = MaxBody + 64<<10
	// frameChunk is the most plaintext one data frame carries; maxFrame
	// is the most ciphertext a reader accepts for one frame.
	frameChunk = 32 << 10
	maxFrame   = 1 << 20
	// WindowSize is how far out of order a sequence number may arrive.
	WindowSize = 64
)

// Header names on the relay-visible outer request and reply.
const (
	HeaderSession = "X-Harness-Session"
	HeaderSeq     = "X-Harness-Seq"
	// HeaderSealed marks a reply whose body is sealed frames; any other
	// reply is a plain refusal (which a relay could fake, so a client
	// only ever treats it as an error, never as data).
	HeaderSealed = "X-Harness-Sealed"
	// HeaderClient is the browser address as the gateway reports it: an
	// unverifiable hint for the audit log, nothing more.
	HeaderClient = "X-Harness-Client"
)

var (
	kdfSalt     = []byte("home-harness remote v1")
	requestAAD  = "home-harness remote v1 request "
	responseAAD = "home-harness remote v1 response "
)

// NewKey returns a fresh remote key, grouped for reading: XXXX-XXXX-...
func NewKey() (string, error) {
	b := make([]byte, KeySymbols)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	var sb strings.Builder
	for i, v := range b {
		if i > 0 && i%4 == 0 {
			sb.WriteByte('-')
		}
		sb.WriteByte(Alphabet[int(v)%len(Alphabet)]) // 256 % 32 == 0: uniform
	}
	return sb.String(), nil
}

// NormalizeKey returns the canonical form of a typed key: upper case,
// without spaces or dashes, with O read as 0 and I or L as 1 (Crockford's
// decoding). remote.js does exactly the same before deriving anything.
func NormalizeKey(key string) (string, error) {
	var sb strings.Builder
	for _, r := range strings.ToUpper(key) {
		switch {
		case r == ' ' || r == '-' || r == '\t':
			continue
		case r == 'O':
			r = '0'
		case r == 'I' || r == 'L':
			r = '1'
		}
		if !strings.ContainsRune(Alphabet, r) {
			return "", errors.New("remoteaccess: a remote key has only letters and digits")
		}
		sb.WriteRune(r)
	}
	if sb.Len() != KeySymbols {
		return "", fmt.Errorf("remoteaccess: a remote key has %d letters and digits", KeySymbols)
	}
	return sb.String(), nil
}

// Secret is what both ends derive from the remote key.
type Secret struct {
	ikm   []byte
	login []byte
}

// NewSecret derives the login key from a remote key (any accepted form).
func NewSecret(key string) (*Secret, error) {
	norm, err := NormalizeKey(key)
	if err != nil {
		return nil, err
	}
	ikm := []byte(norm)
	login, err := hkdf.Key(sha256.New, ikm, kdfSalt, "login", 32)
	if err != nil {
		return nil, err
	}
	return &Secret{ikm: ikm, login: login}, nil
}

func (s *Secret) mac(msg string) []byte {
	m := hmac.New(sha256.New, s.login)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

// ClientProof is the login proof for server nonce sn and client nonce cn.
func (s *Secret) ClientProof(sn, cn string) []byte { return s.mac("client " + sn + " " + cn) }

// ServerProof is the manager's answer, proving it holds the key too.
func (s *Secret) ServerProof(sn, cn string) []byte { return s.mac("server " + sn + " " + cn) }

// SessionKeys derives the session's two directional AES-256-GCM keys,
// bound to both nonces of the login that opened it.
func (s *Secret) SessionKeys(sn, cn string) (c2s, s2c cipher.AEAD, err error) {
	if c2s, err = s.aead("c2s " + sn + " " + cn); err != nil {
		return nil, nil, err
	}
	if s2c, err = s.aead("s2c " + sn + " " + cn); err != nil {
		return nil, nil, err
	}
	return c2s, s2c, nil
}

func (s *Secret) aead(info string) (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, s.ikm, kdfSalt, info, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// nonce is the 12-byte GCM nonce: the call's sequence number, then the
// frame index (0 for the request itself). Each direction has its own key,
// so the same (seq, index) never meets the same key twice.
func nonce(seq uint64, index uint32) []byte {
	n := make([]byte, 12)
	binary.BigEndian.PutUint64(n, seq)
	binary.BigEndian.PutUint32(n[8:], index)
	return n
}

// Request is one operator API call as it travels inside the seal.
type Request struct {
	Method      string `json:"method"`
	Path        string `json:"path"` // path and query, e.g. /audit?limit=5
	ContentType string `json:"type,omitempty"`
	Body        []byte `json:"-"`
}

// SealRequest seals req as call number seq of session sid.
func SealRequest(c2s cipher.AEAD, sid string, seq uint64, req Request) ([]byte, error) {
	if len(req.Body) > MaxBody {
		return nil, fmt.Errorf("remoteaccess: request body over %d MiB", MaxBody>>20)
	}
	head, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	plain := make([]byte, 4, 4+len(head)+len(req.Body))
	binary.BigEndian.PutUint32(plain, uint32(len(head)))
	plain = append(append(plain, head...), req.Body...)
	return c2s.Seal(nil, nonce(seq, 0), plain, []byte(requestAAD+sid)), nil
}

// OpenRequest opens a sealed call; any change to it, its sequence number
// or its session fails here.
func OpenRequest(c2s cipher.AEAD, sid string, seq uint64, sealed []byte) (Request, error) {
	plain, err := c2s.Open(nil, nonce(seq, 0), sealed, []byte(requestAAD+sid))
	if err != nil {
		return Request{}, errors.New("remoteaccess: the request could not be verified")
	}
	if len(plain) < 4 {
		return Request{}, errors.New("remoteaccess: malformed request")
	}
	n := binary.BigEndian.Uint32(plain)
	if uint64(n) > uint64(len(plain)-4) {
		return Request{}, errors.New("remoteaccess: malformed request")
	}
	var req Request
	if err := json.Unmarshal(plain[4:4+n], &req); err != nil {
		return Request{}, errors.New("remoteaccess: malformed request")
	}
	req.Body = plain[4+n:]
	return req, nil
}

// ReplayWindow accepts each sequence number at most once, tolerating
// arrivals up to WindowSize behind the highest seen (the IPsec/DTLS
// anti-replay window). Zero is never valid. Not safe for concurrent use.
type ReplayWindow struct {
	top  uint64
	seen uint64 // bit i: top-i was accepted
}

// Accept reports whether seq is new, and records it if so.
func (w *ReplayWindow) Accept(seq uint64) bool {
	switch {
	case seq == 0:
		return false
	case seq > w.top:
		shift := seq - w.top
		if shift >= WindowSize {
			w.seen = 0
		} else {
			w.seen <<= shift
		}
		w.seen |= 1
		w.top = seq
		return true
	case w.top-seq >= WindowSize:
		return false
	default:
		bit := uint64(1) << (w.top - seq)
		if w.seen&bit != 0 {
			return false
		}
		w.seen |= bit
		return true
	}
}

// Frame kinds inside a sealed reply.
const (
	FrameHead = 'H' // JSON ResponseHead; always first
	FrameData = 'D' // body bytes
	FrameEnd  = 'E' // the reply is complete; always last
)

// ResponseHead is the first frame of a sealed reply.
type ResponseHead struct {
	Status int    `json:"status"`
	Type   string `json:"type,omitempty"`
}

// FrameWriter writes a reply as sealed frames: 4-byte big-endian length,
// then the sealed frame (kind byte + payload) for frame index 0, 1, ...
type FrameWriter struct {
	w     io.Writer
	s2c   cipher.AEAD
	aad   []byte
	seq   uint64
	index uint32
}

// NewFrameWriter starts the reply to call seq of session sid.
func NewFrameWriter(w io.Writer, s2c cipher.AEAD, sid string, seq uint64) *FrameWriter {
	return &FrameWriter{w: w, s2c: s2c, aad: []byte(responseAAD + sid), seq: seq}
}

func (f *FrameWriter) frame(kind byte, payload []byte) error {
	if f.index == ^uint32(0) {
		return errors.New("remoteaccess: reply too long")
	}
	plain := append([]byte{kind}, payload...)
	sealed := f.s2c.Seal(nil, nonce(f.seq, f.index), plain, f.aad)
	f.index++
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(sealed)))
	if _, err := f.w.Write(n[:]); err != nil {
		return err
	}
	_, err := f.w.Write(sealed)
	return err
}

// Head writes the head frame.
func (f *FrameWriter) Head(h ResponseHead) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	return f.frame(FrameHead, b)
}

// Data writes body bytes, split into frames of at most frameChunk.
func (f *FrameWriter) Data(p []byte) error {
	for len(p) > 0 {
		n := min(len(p), frameChunk)
		if err := f.frame(FrameData, p[:n]); err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}

// End writes the end frame; a reply without one is incomplete.
func (f *FrameWriter) End() error { return f.frame(FrameEnd, nil) }

// FrameReader opens the frames FrameWriter wrote, in order.
type FrameReader struct {
	r     io.Reader
	s2c   cipher.AEAD
	aad   []byte
	seq   uint64
	index uint32
	ended bool
}

// NewFrameReader reads the reply to call seq of session sid.
func NewFrameReader(r io.Reader, s2c cipher.AEAD, sid string, seq uint64) *FrameReader {
	return &FrameReader{r: r, s2c: s2c, aad: []byte(responseAAD + sid), seq: seq}
}

// ErrCutShort means the reply ended before its end frame.
var ErrCutShort = errors.New("remoteaccess: the reply was cut short")

// Next returns the next frame's kind and payload.
func (f *FrameReader) Next() (byte, []byte, error) {
	if f.ended {
		return 0, nil, io.EOF
	}
	var n [4]byte
	if _, err := io.ReadFull(f.r, n[:]); err != nil {
		return 0, nil, ErrCutShort
	}
	size := binary.BigEndian.Uint32(n[:])
	if size > maxFrame {
		return 0, nil, errors.New("remoteaccess: reply frame too large")
	}
	sealed := make([]byte, size)
	if _, err := io.ReadFull(f.r, sealed); err != nil {
		return 0, nil, ErrCutShort
	}
	plain, err := f.s2c.Open(nil, nonce(f.seq, f.index), sealed, f.aad)
	if err != nil || len(plain) == 0 {
		return 0, nil, errors.New("remoteaccess: the reply could not be verified")
	}
	f.index++
	kind := plain[0]
	if (f.index == 1) != (kind == FrameHead) {
		return 0, nil, errors.New("remoteaccess: malformed reply")
	}
	if kind == FrameEnd {
		f.ended = true
	}
	return kind, plain[1:], nil
}

// ReadHead reads the head frame that starts every reply.
func (f *FrameReader) ReadHead() (ResponseHead, error) {
	kind, payload, err := f.Next()
	if err != nil {
		return ResponseHead{}, err
	}
	if kind != FrameHead {
		return ResponseHead{}, errors.New("remoteaccess: malformed reply")
	}
	var h ResponseHead
	if err := json.Unmarshal(payload, &h); err != nil || h.Status < 100 || h.Status > 599 {
		return ResponseHead{}, errors.New("remoteaccess: malformed reply")
	}
	return h, nil
}

// Body returns the rest of the reply as a reader of its body bytes: it
// fails with ErrCutShort (or a verification error) instead of ever
// reporting a clean end the manager did not send.
func (f *FrameReader) Body() io.Reader { return &bodyReader{f: f} }

type bodyReader struct {
	f   *FrameReader
	buf []byte
}

func (b *bodyReader) Read(p []byte) (int, error) {
	for len(b.buf) == 0 {
		kind, payload, err := b.f.Next()
		if err != nil {
			return 0, err
		}
		switch kind {
		case FrameData:
			b.buf = payload
		case FrameEnd:
			return 0, io.EOF
		default:
			return 0, errors.New("remoteaccess: malformed reply")
		}
	}
	n := copy(p, b.buf)
	b.buf = b.buf[n:]
	return n, nil
}

// Equal compares two MACs in constant time.
func Equal(a, b []byte) bool { return hmac.Equal(a, b) }

// trimmed is a small helper for error bodies.
func trimmed(b []byte) string { return string(bytes.TrimSpace(b)) }

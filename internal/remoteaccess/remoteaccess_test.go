package remoteaccess

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestNewKeyShapeAndNormalize(t *testing.T) {
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != 29 || strings.Count(key, "-") != 5 {
		t.Fatalf("key %q: want 6 groups of 4", key)
	}
	norm, err := NormalizeKey(key)
	if err != nil || len(norm) != KeySymbols {
		t.Fatalf("NormalizeKey(%q) = %q, %v", key, norm, err)
	}
	// Typed sloppily: lower case, spaces, O for 0, l for 1.
	if got, err := NormalizeKey("o1ab cdef-ghjk mnpq rstv wxyl"); err != nil || got != "01ABCDEFGHJKMNPQRSTVWXY1" {
		t.Fatalf("NormalizeKey = %q, %v", got, err)
	}
	for _, bad := range []string{"", "short", strings.Repeat("A", 25), strings.Repeat("U", 24), strings.Repeat("A", 23) + "!"} {
		if _, err := NormalizeKey(bad); err == nil {
			t.Errorf("NormalizeKey(%q) accepted", bad)
		}
	}
	other, _ := NewKey()
	if other == key {
		t.Fatal("two fresh keys are equal")
	}
}

// TestKnownVector pins the derivation, so remote.js (which must match it
// byte for byte) can be checked against the same numbers.
func TestKnownVector(t *testing.T) {
	s, err := NewSecret("0123-4567-89AB-CDEF-GHJK-MNPQ")
	if err != nil {
		t.Fatal(err)
	}
	sn, cn := "00112233", "aabbccdd"
	if got := hex.EncodeToString(s.ClientProof(sn, cn)); got != knownClientProof {
		t.Errorf("client proof = %s", got)
	}
	if got := hex.EncodeToString(s.ServerProof(sn, cn)); got != knownServerProof {
		t.Errorf("server proof = %s", got)
	}
	c2s, _, err := s.SessionKeys(sn, cn)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := SealRequest(c2s, "sid", 7, Request{Method: "GET", Path: "/nodes"})
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(sealed); got != knownSealedRequest {
		t.Errorf("sealed request = %s", got)
	}
}

// Computed independently with WebCrypto (Node 22's crypto.subtle) using
// remote.js's derivation, nonce layout and additional data.
const (
	knownClientProof   = "9f1639cfbb9dc4eefc0882242b3ea075d996de5c1a20b46d2c153c4d9cbee979"
	knownServerProof   = "efa4f5a2ab83d4a5e387fc46e39f3ba7c8af32c0fe5648e2f67faed2025d194e"
	knownSealedRequest = "aa9bd762711a765eeb9979f2e3b1e30e70ba7a1fc2f863efb6a69d560f5eceee1459d99c29a6a8f8164a380995a03f7ce7f0782b"
)

func TestNonceLayout(t *testing.T) {
	if got := hex.EncodeToString(nonce(1<<32+5, 3)); got != "000000010000000500000003" {
		t.Fatalf("nonce = %s (remote.js gives 000000010000000500000003)", got)
	}
}

func session(t *testing.T) (*Secret, string, string) {
	t.Helper()
	key, _ := NewKey()
	s, err := NewSecret(key)
	if err != nil {
		t.Fatal(err)
	}
	return s, "server-nonce", "client-nonce"
}

func TestProofsDependOnKeyAndNonces(t *testing.T) {
	s, sn, cn := session(t)
	other, _, _ := session(t)
	p := s.ClientProof(sn, cn)
	switch {
	case Equal(p, other.ClientProof(sn, cn)):
		t.Fatal("another key gives the same proof")
	case Equal(p, s.ClientProof(sn+"x", cn)):
		t.Fatal("another server nonce gives the same proof")
	case Equal(p, s.ClientProof(sn, cn+"x")):
		t.Fatal("another client nonce gives the same proof")
	case Equal(p, s.ServerProof(sn, cn)):
		t.Fatal("the client's proof doubles as the server's")
	}
}

func TestRequestRoundTripAndTampering(t *testing.T) {
	s, sn, cn := session(t)
	c2s, s2c, err := s.SessionKeys(sn, cn)
	if err != nil {
		t.Fatal(err)
	}
	req := Request{Method: "POST", Path: "/workloads?x=1", ContentType: "application/json", Body: []byte(`{"command":"hostname"}`)}
	sealed, err := SealRequest(c2s, "sid-1", 5, req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := OpenRequest(c2s, "sid-1", 5, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if got.Method != req.Method || got.Path != req.Path || got.ContentType != req.ContentType || !bytes.Equal(got.Body, req.Body) {
		t.Fatalf("round trip = %+v", got)
	}
	if bytes.Contains(sealed, []byte("workloads")) || bytes.Contains(sealed, []byte("hostname")) {
		t.Fatal("the sealed request shows its path or body")
	}
	for i := range sealed {
		bad := bytes.Clone(sealed)
		bad[i] ^= 0x01
		if _, err := OpenRequest(c2s, "sid-1", 5, bad); err == nil {
			t.Fatalf("a flipped byte %d was accepted", i)
		}
	}
	if _, err := OpenRequest(c2s, "sid-1", 6, sealed); err == nil {
		t.Fatal("accepted under another sequence number")
	}
	if _, err := OpenRequest(c2s, "sid-2", 5, sealed); err == nil {
		t.Fatal("accepted for another session")
	}
	if _, err := OpenRequest(s2c, "sid-1", 5, sealed); err == nil {
		t.Fatal("the reply key opens requests")
	}
	other, _, _ := session(t)
	oc2s, _, _ := other.SessionKeys(sn, cn)
	if _, err := OpenRequest(oc2s, "sid-1", 5, sealed); err == nil {
		t.Fatal("another key opens it")
	}
	if _, err := SealRequest(c2s, "sid-1", 9, Request{Method: "POST", Path: "/", Body: make([]byte, MaxBody+1)}); err == nil {
		t.Fatal("an oversized body was sealed")
	}
}

func TestReplayWindow(t *testing.T) {
	var w ReplayWindow
	accept := func(seq uint64, want bool) {
		t.Helper()
		if got := w.Accept(seq); got != want {
			t.Fatalf("Accept(%d) = %v, want %v", seq, got, want)
		}
	}
	accept(0, false)
	accept(1, true)
	accept(1, false) // replay
	accept(3, true)
	accept(2, true) // late but new: parallel requests
	accept(2, false)
	accept(3, false)
	accept(100, true)
	accept(100-WindowSize+1, true)  // oldest still inside the window
	accept(100-WindowSize+1, false) // and only once
	accept(100-WindowSize, false)   // fell out of the window
	accept(99, true)
	accept(1000, true) // a jump past the window forgets everything older
	accept(999, true)
	accept(1000, false)
}

func TestFramesRoundTripStreamAndTampering(t *testing.T) {
	s, sn, cn := session(t)
	_, s2c, err := s.SessionKeys(sn, cn)
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte("0123456789"), 10000) // several data frames
	var wire bytes.Buffer
	fw := NewFrameWriter(&wire, s2c, "sid", 3)
	if err := fw.Head(ResponseHead{Status: 201, Type: "application/json"}); err != nil {
		t.Fatal(err)
	}
	fw.Data(body)
	fw.End()
	full := wire.Bytes()

	read := func(data []byte, sid string, seq uint64) (ResponseHead, []byte, error) {
		fr := NewFrameReader(bytes.NewReader(data), s2c, sid, seq)
		h, err := fr.ReadHead()
		if err != nil {
			return h, nil, err
		}
		b, err := io.ReadAll(fr.Body())
		return h, b, err
	}
	h, got, err := read(full, "sid", 3)
	if err != nil || h.Status != 201 || h.Type != "application/json" || !bytes.Equal(got, body) {
		t.Fatalf("round trip: head %+v, %d bytes, %v", h, len(got), err)
	}
	if _, _, err := read(full, "sid", 4); err == nil {
		t.Fatal("a reply opened as the answer to another call")
	}
	if _, _, err := read(full, "other", 3); err == nil {
		t.Fatal("a reply opened for another session")
	}
	// Cut short anywhere, including right before the end frame: never a
	// clean end.
	for _, n := range []int{len(full) - 1, len(full) - 25, len(full) / 2, 10} {
		if _, _, err := read(full[:n], "sid", 3); err == nil {
			t.Fatalf("a reply cut to %d of %d bytes read cleanly", n, len(full))
		}
	}
	for _, i := range []int{5, 40, len(full) / 2, len(full) - 3} {
		bad := bytes.Clone(full)
		bad[i] ^= 0x80
		if _, _, err := read(bad, "sid", 3); err == nil {
			t.Fatalf("a flipped byte %d was accepted", i)
		}
	}
	// Dropping a whole data frame (frames are numbered) fails too.
	fr := NewFrameReader(bytes.NewReader(full), s2c, "sid", 3)
	if _, err := fr.ReadHead(); err != nil {
		t.Fatal(err)
	}
	headLen := len(full) - fr.r.(*bytes.Reader).Len()
	frameLen := 4 + int(uint32(full[headLen])<<24|uint32(full[headLen+1])<<16|uint32(full[headLen+2])<<8|uint32(full[headLen+3]))
	dropped := append(bytes.Clone(full[:headLen]), full[headLen+frameLen:]...)
	if _, _, err := read(dropped, "sid", 3); err == nil {
		t.Fatal("a reply with a frame removed was accepted")
	}
	// A reply with no body still needs its end frame.
	wire.Reset()
	fw = NewFrameWriter(&wire, s2c, "sid", 8)
	fw.Head(ResponseHead{Status: 204})
	if _, _, err := read(wire.Bytes(), "sid", 8); !errors.Is(err, ErrCutShort) {
		t.Fatalf("missing end frame: %v", err)
	}
}

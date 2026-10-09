package remoteaccess

import (
	"bytes"
	"testing"
)

func fuzzAEAD(t testing.TB) (open func(sid string, seq uint64, sealed []byte) (Request, error), seal func(sid string, seq uint64, r Request) ([]byte, error)) {
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	sec, err := NewSecret(key)
	if err != nil {
		t.Fatal(err)
	}
	a, _, err := sec.SessionKeys("server-nonce", "client-nonce")
	if err != nil {
		t.Fatal(err)
	}
	return 		func(sid string, seq uint64, sealed []byte) (Request, error) { return OpenRequest(a, sid, seq, sealed) },
		func(sid string, seq uint64, r Request) ([]byte, error) { return SealRequest(a, sid, seq, r) }
}

// FuzzOpenRequest: arbitrary bytes presented as a sealed call never panic
// and are never accepted (the attacker holds no key).
func FuzzOpenRequest(f *testing.F) {
	open, seal := fuzzAEAD(f)
	good, _ := seal("sid", 1, Request{Method: "GET", Path: "/nodes"})
	f.Add("sid", uint64(1), good)
	f.Add("sid", uint64(2), []byte{})
	f.Add("", uint64(0), []byte{0, 0, 0, 9})
	f.Fuzz(func(t *testing.T, sid string, seq uint64, sealed []byte) {
		if bytes.Equal(sealed, good) && sid == "sid" && seq == 1 {
			return
		}
		if _, err := open(sid, seq, sealed); err == nil {
			t.Fatalf("a forged sealed request was accepted (sid %q seq %d)", sid, seq)
		}
	})
}

// FuzzSealOpenRoundTrip: what is sealed opens to the same call, and a
// change of session or sequence number fails.
func FuzzSealOpenRoundTrip(f *testing.F) {
	open, seal := fuzzAEAD(f)
	f.Add("GET", "/nodes", "", []byte(nil))
	f.Add("POST", "/jobs", "application/json", []byte(`{"a":1}`))
	f.Fuzz(func(t *testing.T, method, path, ctype string, body []byte) {
		sealed, err := seal("s1", 7, Request{Method: method, Path: path, ContentType: ctype, Body: body})
		if err != nil {
			return
		}
		got, err := open("s1", 7, sealed)
		if err != nil || got.Method != method || got.Path != path || got.ContentType != ctype || !bytes.Equal(got.Body, body) {
			t.Fatalf("round trip failed: %+v %v", got, err)
		}
		if _, err := open("s2", 7, sealed); err == nil {
			t.Fatal("opened under another session")
		}
		if _, err := open("s1", 8, sealed); err == nil {
			t.Fatal("opened under another sequence number")
		}
	})
}

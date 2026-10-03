package protocol

import (
	"regexp"
	"strings"
	"testing"
)

func TestPairingCode(t *testing.T) {
	fp := strings.Repeat("ab", 32)
	key := []byte("0123456789abcdef0123456789abcdef")
	code := PairingCode(fp, key)
	if !regexp.MustCompile(`^\d{3} \d{3}$`).MatchString(code) {
		t.Fatalf("code %q is not two groups of three digits", code)
	}
	if PairingCode(fp, key) != code {
		t.Fatal("the code must be the same every time, on both sides")
	}
	if PairingCode(" "+strings.ToUpper(fp)+"\n", key) != code {
		t.Fatal("the fingerprint's case and surrounding space must not matter")
	}
	// A machine in the middle shows the device its own certificate, and
	// the manager its own key: either change must change the code.
	if PairingCode(strings.Repeat("cd", 32), key) == code {
		t.Fatal("a different manager certificate gave the same code")
	}
	if PairingCode(fp, []byte("another key, another device.....")) == code {
		t.Fatal("a different device key gave the same code")
	}
}

package relay

import "testing"

func TestAliasIsOpaqueAuthenticatedAndRestartStable(t *testing.T) {
	aead, err := newAliasCipher("stable-alias-secret")
	if err != nil {
		t.Fatal(err)
	}
	alias, err := sealAlias(aead, "private-manager-session")
	if err != nil {
		t.Fatal(err)
	}
	if alias == "private-manager-session" {
		t.Fatal("alias exposed the manager session")
	}
	if got, ok := openAlias(aead, alias); !ok || got != "private-manager-session" {
		t.Fatalf("alias did not round-trip: got=%q ok=%v", got, ok)
	}
	restarted, _ := newAliasCipher("stable-alias-secret")
	if got, ok := openAlias(restarted, alias); !ok || got != "private-manager-session" {
		t.Fatal("alias did not survive recreation with the same key")
	}
	pos := len(alias) / 2
	replacement := byte('A')
	if alias[pos] == replacement {
		replacement = 'B'
	}
	tampered := alias[:pos] + string(replacement) + alias[pos+1:]
	if _, ok := openAlias(aead, tampered); ok {
		t.Fatal("tampered alias was accepted")
	}
	wrongKey, _ := newAliasCipher("different-secret")
	if _, ok := openAlias(wrongKey, alias); ok {
		t.Fatal("alias opened with the wrong key")
	}
}

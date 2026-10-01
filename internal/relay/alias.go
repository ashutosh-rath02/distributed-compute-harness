package relay

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
)

const aliasPrefix = "ha1."

type aliasPayload struct {
	Session string `json:"session"`
}

func newAliasCipher(secret string) (cipher.AEAD, error) {
	if secret == "" {
		return nil, nil
	}
	key := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func sealAlias(aead cipher.AEAD, session string) (string, error) {
	if aead == nil || session == "" {
		return "", errors.New("relay: device aliases are not configured")
	}
	plain, err := json.Marshal(aliasPayload{Session: session})
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nonce, nonce, plain, []byte(aliasPrefix))
	return aliasPrefix + base64.RawURLEncoding.EncodeToString(sealed), nil
}

func openAlias(aead cipher.AEAD, token string) (string, bool) {
	if aead == nil || len(token) <= len(aliasPrefix) || token[:len(aliasPrefix)] != aliasPrefix {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(token[len(aliasPrefix):])
	if err != nil || len(raw) < aead.NonceSize() {
		return "", false
	}
	nonce, ciphertext := raw[:aead.NonceSize()], raw[aead.NonceSize():]
	plain, err := aead.Open(nil, nonce, ciphertext, []byte(aliasPrefix))
	if err != nil {
		return "", false
	}
	var payload aliasPayload
	if json.Unmarshal(plain, &payload) != nil || payload.Session == "" {
		return "", false
	}
	return payload.Session, true
}

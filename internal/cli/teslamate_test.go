package cli

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"testing"
)

// TestCloakEncryptRoundTrip pins TeslaMate's Cloak "AES.GCM.V1" blob format and
// proves a value we write decrypts under the same key + AAD TeslaMate uses. If
// this drifts, `teslamate auth` writes a row TeslaMate cannot read.
func TestCloakEncryptRoundTrip(t *testing.T) {
	key := sha256.Sum256([]byte("an-encryption-key"))
	plaintext := []byte("qts-local")

	blob, err := cloakEncrypt(key[:], plaintext)
	if err != nil {
		t.Fatalf("cloakEncrypt: %v", err)
	}

	// Header: <<1, 10>> "AES.GCM.V1".
	const tag = "AES.GCM.V1"
	wantHeader := append([]byte{1, byte(len(tag))}, tag...)
	if !bytes.HasPrefix(blob, wantHeader) {
		t.Fatalf("blob header = %x, want prefix %x", blob[:len(wantHeader)], wantHeader)
	}

	// Layout: header | iv(12) | ciphertag(16) | ciphertext. Decrypt as TeslaMate would.
	rest := blob[len(wantHeader):]
	if len(rest) < 12+16 {
		t.Fatalf("blob too short: %d bytes after header", len(rest))
	}
	iv, ciphertag, ciphertext := rest[:12], rest[12:28], rest[28:]

	block, err := aes.NewCipher(key[:])
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, 12)
	if err != nil {
		t.Fatal(err)
	}
	got, err := gcm.Open(nil, iv, append(append([]byte{}, ciphertext...), ciphertag...), []byte(tag))
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("decrypted = %q, want %q", got, plaintext)
	}
}

func TestCheckPredicatesCoverStates(t *testing.T) {
	for _, state := range []string{"online", "driving", "charging", "parked"} {
		if _, ok := checkPredicates[state]; !ok {
			t.Errorf("missing check predicate for %q", state)
		}
	}
}

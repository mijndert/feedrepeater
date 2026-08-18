package secret

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func testKeyring(t *testing.T) *Keyring {
	t.Helper()
	root := make([]byte, 32)
	if _, err := rand.Read(root); err != nil {
		t.Fatal(err)
	}
	k, err := NewKeyring(root)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestNewKeyringRequires32Bytes(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33, 64} {
		if _, err := NewKeyring(make([]byte, n)); err == nil {
			t.Errorf("NewKeyring(%d bytes) = nil error", n)
		}
	}
}

func TestEncryptRoundTrip(t *testing.T) {
	k := testKeyring(t)
	plain := "a-mastodon-access-token"

	sealed, err := k.EncryptString(PurposeUserToken, plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte(plain)) {
		t.Error("ciphertext contains the plaintext")
	}
	got, err := k.DecryptString(PurposeUserToken, sealed)
	if err != nil || got != plain {
		t.Errorf("round trip = %q, %v", got, err)
	}
}

// A ciphertext moved between columns must not decrypt, so a write primitive in
// one place cannot promote a value into another.
func TestPurposeIsBound(t *testing.T) {
	k := testKeyring(t)
	sealed, err := k.EncryptString(PurposeDestination, "webhook-secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Decrypt(PurposeClientSecret, sealed); err == nil {
		t.Error("ciphertext decrypted under the wrong purpose")
	}
}

func TestNoncesAreUnique(t *testing.T) {
	k := testKeyring(t)
	seen := map[string]bool{}
	for range 200 {
		sealed, err := k.EncryptString(PurposeUserToken, "same plaintext every time")
		if err != nil {
			t.Fatal(err)
		}
		nonce := string(sealed[:12])
		if seen[nonce] {
			t.Fatal("nonce reused, which breaks AES-GCM")
		}
		seen[nonce] = true
	}
}

func TestTamperingIsDetected(t *testing.T) {
	k := testKeyring(t)
	sealed, err := k.EncryptString(PurposeUserToken, "token")
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{0, len(sealed) / 2, len(sealed) - 1} {
		bad := bytes.Clone(sealed)
		bad[i] ^= 0xff
		if _, err := k.Decrypt(PurposeUserToken, bad); err == nil {
			t.Errorf("tampered byte %d decrypted", i)
		}
	}
	if _, err := k.Decrypt(PurposeUserToken, nil); err == nil {
		t.Error("empty ciphertext decrypted")
	}
	if _, err := k.Decrypt(PurposeUserToken, []byte("short")); err == nil {
		t.Error("truncated ciphertext decrypted")
	}
}

func TestDifferentRootKeysDoNotInterop(t *testing.T) {
	a, b := testKeyring(t), testKeyring(t)
	sealed, err := a.EncryptString(PurposeUserToken, "token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Decrypt(PurposeUserToken, sealed); err == nil {
		t.Error("another key's ciphertext decrypted")
	}
}

func TestCSRFTokensAreSessionBound(t *testing.T) {
	k := testKeyring(t)
	a := k.CSRFToken("session-a")
	b := k.CSRFToken("session-b")

	if a == b {
		t.Error("two sessions share a CSRF token")
	}
	if a != k.CSRFToken("session-a") {
		t.Error("CSRF token is not stable for a session")
	}
	if !k.ValidCSRF("session-a", a) {
		t.Error("valid token rejected")
	}
	if k.ValidCSRF("session-a", b) {
		t.Error("another session's token accepted")
	}
	if k.ValidCSRF("session-a", "") {
		t.Error("empty token accepted")
	}
	if k.ValidCSRF("session-a", a[:len(a)-1]) {
		t.Error("truncated token accepted")
	}
}

func TestTokensAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		tok := Token()
		if len(tok) < 40 {
			t.Fatalf("token is only %d characters", len(tok))
		}
		if seen[tok] {
			t.Fatal("duplicate token")
		}
		seen[tok] = true
	}
}

func TestHashIsNotReversibleToTheToken(t *testing.T) {
	tok := Token()
	h := Hash(tok)
	if h == tok {
		t.Error("Hash returned the token")
	}
	if h != Hash(tok) {
		t.Error("Hash is not deterministic")
	}
	if len(h) != 64 {
		t.Errorf("Hash returned %d characters, want 64", len(h))
	}
}

func TestGenerateRootKeyIsUsable(t *testing.T) {
	hexKey := GenerateRootKey()
	if len(hexKey) != 64 {
		t.Fatalf("root key is %d characters, want 64", len(hexKey))
	}
	if hexKey == GenerateRootKey() {
		t.Fatal("root keys repeat")
	}
}

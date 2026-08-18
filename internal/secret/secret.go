// Package secret handles key derivation, authenticated encryption of stored
// credentials, and token generation.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

// Purpose binds a ciphertext to the column it lives in. A value encrypted as
// one purpose will not decrypt as another, so a database write primitive
// cannot be used to move a webhook secret into an OAuth client secret slot.
type Purpose string

const (
	PurposeUserToken    Purpose = "user-access-token"
	PurposeClientSecret Purpose = "oauth-client-secret"
	PurposeDestination  Purpose = "destination-credentials"
	PurposePKCEVerifier Purpose = "pkce-verifier"
)

var ErrDecrypt = errors.New("secret: decryption failed")

// Keyring derives every subkey the application needs from one root key.
type Keyring struct {
	aead    cipher.AEAD
	csrfKey []byte
}

// NewKeyring derives subkeys from 32 bytes of root key material.
func NewKeyring(root []byte) (*Keyring, error) {
	if len(root) != 32 {
		return nil, fmt.Errorf("secret: root key must be 32 bytes, got %d", len(root))
	}
	encKey, err := hkdf.Key(sha256.New, root, nil, "feedrepeater/v1/column-encryption", 32)
	if err != nil {
		return nil, err
	}
	csrfKey, err := hkdf.Key(sha256.New, root, nil, "feedrepeater/v1/csrf", 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Keyring{aead: aead, csrfKey: csrfKey}, nil
}

// Encrypt seals plaintext under the given purpose. The output is
// nonce || ciphertext || tag, with the nonce drawn fresh from crypto/rand for
// every call.
func (k *Keyring) Encrypt(p Purpose, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return k.aead.Seal(nonce, nonce, plaintext, []byte(p)), nil
}

// EncryptString is Encrypt for string plaintext.
func (k *Keyring) EncryptString(p Purpose, s string) ([]byte, error) {
	return k.Encrypt(p, []byte(s))
}

// Decrypt opens a ciphertext produced by Encrypt under the same purpose.
func (k *Keyring) Decrypt(p Purpose, sealed []byte) ([]byte, error) {
	n := k.aead.NonceSize()
	if len(sealed) < n+k.aead.Overhead() {
		return nil, ErrDecrypt
	}
	out, err := k.aead.Open(nil, sealed[:n], sealed[n:], []byte(p))
	if err != nil {
		return nil, ErrDecrypt
	}
	return out, nil
}

// DecryptString is Decrypt returning a string.
func (k *Keyring) DecryptString(p Purpose, sealed []byte) (string, error) {
	b, err := k.Decrypt(p, sealed)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// CSRFToken derives a deterministic, session-bound CSRF token. Because it is a
// pure function of the session ID it needs no server-side storage and is
// invalidated automatically when the session ends.
func (k *Keyring) CSRFToken(sessionID string) string {
	mac := hmac.New(sha256.New, k.csrfKey)
	mac.Write([]byte(sessionID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// ValidCSRF compares a submitted token against the expected one in constant time.
func (k *Keyring) ValidCSRF(sessionID, submitted string) bool {
	if submitted == "" {
		return false
	}
	return hmac.Equal([]byte(k.CSRFToken(sessionID)), []byte(submitted))
}

// Token returns a URL-safe random token with 256 bits of entropy.
func Token() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("secret: crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Hash returns the hex SHA-256 of a token. Session and webhook tokens are
// stored hashed so a database leak does not hand over live credentials.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// GenerateRootKey returns a new hex-encoded root key for FR_SECRET_KEY.
func GenerateRootKey() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("secret: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

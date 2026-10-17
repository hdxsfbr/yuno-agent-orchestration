package project

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
)

// Cipher seals project tokens at rest (DESIGN 5.17 rule 2): AES-256-GCM under
// SHA-256(PROJECT_TOKEN_KEY), a random 96-bit nonce in front of the ciphertext, and the
// project id as additional data, so a blob opens only for the row it was sealed for.
type Cipher struct {
	aead cipher.AEAD
}

// ErrNoKey is the answer when PROJECT_TOKEN_KEY is empty: no token can be stored or
// read.
var ErrNoKey = errors.New("PROJECT_TOKEN_KEY is not set")

// ErrCannotRead is the answer when a stored blob does not open: the key changed, or the
// row's blob is not one of ours. The operator fixes it by entering the token again.
var ErrCannotRead = errors.New("the stored token cannot be read; enter it again on the Projects page")

// NewCipher derives the cipher from PROJECT_TOKEN_KEY; ErrNoKey for an empty value.
func NewCipher(key string) (*Cipher, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, ErrNoKey
	}
	sum := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

// Seal encrypts token for the project; the result is nonce || ciphertext.
func (c *Cipher) Seal(projectID, token string) ([]byte, error) {
	if c == nil {
		return nil, ErrNoKey
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return c.aead.Seal(nonce, nonce, []byte(token), []byte(projectID)), nil
}

// Open decrypts a blob sealed for the project; ErrCannotRead for anything that does
// not open, with no detail (the reason is never worth leaking).
func (c *Cipher) Open(projectID string, blob []byte) (string, error) {
	if c == nil {
		return "", ErrNoKey
	}
	n := c.aead.NonceSize()
	if len(blob) < n+c.aead.Overhead() {
		return "", ErrCannotRead
	}
	plain, err := c.aead.Open(nil, blob[:n], blob[n:], []byte(projectID))
	if err != nil {
		return "", ErrCannotRead
	}
	return string(plain), nil
}

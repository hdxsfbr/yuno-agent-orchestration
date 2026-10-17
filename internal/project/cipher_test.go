package project

import (
	"bytes"
	"errors"
	"testing"
)

// T5.17.1 (cipher): a token round-trips under its project id and nothing else opens it.
func TestCipherRoundTrip(t *testing.T) {
	c, err := NewCipher("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	const token = "glpat-not-a-real-token-00"
	blob, err := c.Seal("project-1", token)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, []byte(token)) || bytes.Contains(blob, []byte("not-a-real")) {
		t.Fatal("the blob carries the token in clear")
	}
	if got, err := c.Open("project-1", blob); err != nil || got != token {
		t.Fatalf("open = %q %v", got, err)
	}
	again, _ := c.Seal("project-1", token)
	if bytes.Equal(again, blob) {
		t.Fatal("two seals of one token are equal: the nonce is not random")
	}
	if _, err := c.Open("project-2", blob); !errors.Is(err, ErrCannotRead) {
		t.Fatalf("another project's id opened the blob: %v", err)
	}
	other, _ := NewCipher("another key")
	if _, err := other.Open("project-1", blob); !errors.Is(err, ErrCannotRead) {
		t.Fatalf("another key opened the blob: %v", err)
	}
	if _, err := c.Open("project-1", blob[:len(blob)-1]); !errors.Is(err, ErrCannotRead) {
		t.Fatalf("a damaged blob opened: %v", err)
	}
	if _, err := c.Open("project-1", []byte("short")); !errors.Is(err, ErrCannotRead) {
		t.Fatalf("a short blob opened: %v", err)
	}
	// No key: nothing can be sealed or opened.
	if _, err := NewCipher("  "); !errors.Is(err, ErrNoKey) {
		t.Fatalf("empty key: %v", err)
	}
	var none *Cipher
	if _, err := none.Seal("p", token); !errors.Is(err, ErrNoKey) {
		t.Fatalf("seal without a key: %v", err)
	}
	if _, err := none.Open("p", blob); !errors.Is(err, ErrNoKey) {
		t.Fatalf("open without a key: %v", err)
	}
}

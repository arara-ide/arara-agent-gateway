package secret

import (
	"bytes"
	"strings"
	"testing"
)

func box(t *testing.T) *Box {
	t.Helper()
	b, err := New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRoundTrip(t *testing.T) {
	b := box(t)
	sealed, err := b.Seal([]byte("gho_secret"), "user-a")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed, "gho_secret") {
		t.Fatal("ciphertext leaks plaintext")
	}
	plain, err := b.Open(sealed, "user-a")
	if err != nil || string(plain) != "gho_secret" {
		t.Fatalf("open: %q %v", plain, err)
	}
}

func TestAssociatedDataBindsOwner(t *testing.T) {
	b := box(t)
	sealed, _ := b.Seal([]byte("token"), "user-a")
	if _, err := b.Open(sealed, "user-b"); err == nil {
		t.Fatal("ciphertext of user-a opened as user-b")
	}
}

func TestTamperAndWrongKey(t *testing.T) {
	b := box(t)
	sealed, _ := b.Seal([]byte("token"), "u")
	tampered := sealed[:len(sealed)-2] + "AA"
	if _, err := b.Open(tampered, "u"); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
	other, _ := New(bytes.Repeat([]byte{8}, 32))
	if _, err := other.Open(sealed, "u"); err == nil {
		t.Fatal("wrong key accepted")
	}
	if _, err := New([]byte("short")); err == nil {
		t.Fatal("short key accepted")
	}
}

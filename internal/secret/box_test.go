package secret

import (
	"bytes"
	"testing"
)

func testBox(t *testing.T) *Box {
	t.Helper()
	b, err := New(bytes.Repeat([]byte{7}, 32), "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

func TestRoundTrip(t *testing.T) {
	b := testBox(t)
	const want = "hunter2 🔐"
	ct, err := b.Seal(want)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(ct, []byte("hunter2")) {
		t.Fatal("plaintext is visible in the ciphertext")
	}
	got, err := b.Open(ct)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestNonceIsFresh guards the failure that makes GCM catastrophic: reusing a
// nonce across messages.
func TestNonceIsFresh(t *testing.T) {
	b := testBox(t)
	a, _ := b.Seal("same")
	c, _ := b.Seal("same")
	if bytes.Equal(a, c) {
		t.Fatal("identical plaintexts produced identical ciphertexts; the nonce is being reused")
	}
}

func TestWrongKeyFails(t *testing.T) {
	ct, err := testBox(t).Seal("secret")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	other, err := New(bytes.Repeat([]byte{9}, 32), "other")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := other.Open(ct); err == nil {
		t.Fatal("decryption with the wrong key succeeded")
	}
}

func TestRejectsBadKeyLength(t *testing.T) {
	if _, err := New([]byte("short"), "x"); err == nil {
		t.Fatal("accepted a key that is not 32 bytes")
	}
}

func TestOpenRejectsTruncated(t *testing.T) {
	if _, err := testBox(t).Open([]byte{1, 2, 3}); err == nil {
		t.Fatal("accepted a ciphertext shorter than its nonce")
	}
}

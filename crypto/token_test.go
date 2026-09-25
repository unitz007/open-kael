package crypto_test

import (
	"context"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/unitz007/open-kael/crypto"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, crypto.KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := testKey(t)
	plaintext := "ghu_realGitHubTokenLookingString123"

	encrypted, err := crypto.Encrypt(key, plaintext)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if encrypted == plaintext {
		t.Fatal("encrypted value should not equal plaintext")
	}
	if strings.Contains(encrypted, plaintext) {
		t.Fatal("encrypted value should not contain the plaintext")
	}

	decrypted, err := crypto.Decrypt(key, encrypted)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if decrypted != plaintext {
		t.Fatalf("expected %q, got %q", plaintext, decrypted)
	}
}

func TestEncryptIsNotDeterministic(t *testing.T) {
	key := testKey(t)
	a, _ := crypto.Encrypt(key, "same input")
	b, _ := crypto.Encrypt(key, "same input")
	if a == b {
		t.Fatal("expected two encryptions of the same plaintext to differ (random nonce)")
	}
}

func TestDecryptWithWrongKeyFails(t *testing.T) {
	key := testKey(t)
	wrongKey := testKey(t)

	encrypted, err := crypto.Encrypt(key, "secret")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := crypto.Decrypt(wrongKey, encrypted); err == nil {
		t.Fatal("expected decryption with the wrong key to fail")
	}
}

func TestResolverDecrypts(t *testing.T) {
	key := testKey(t)
	encrypted, err := crypto.Encrypt(key, "a-real-token")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	resolver := crypto.NewResolver(key)
	token, err := resolver.ResolveToken(context.Background(), encrypted)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if token != "a-real-token" {
		t.Fatalf("unexpected token: %q", token)
	}
}

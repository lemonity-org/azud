package release

import (
	"strings"
	"testing"
)

func TestSecretsDecryptWithAnyRecipientsIdentity(t *testing.T) {
	serverID, serverRecipient, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	backupID, backupRecipient, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("DATABASE_URL=postgres://app\nSECRET_KEY_BASE=abc\n")

	ciphertext, err := Encrypt(plaintext, []string{serverRecipient, backupRecipient})
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if strings.Contains(string(ciphertext), "postgres://") {
		t.Fatal("ciphertext contains the plaintext")
	}
	for name, id := range map[string][]byte{"server": serverID, "backup": backupID} {
		got, err := Decrypt(ciphertext, id)
		if err != nil || string(got) != string(plaintext) {
			t.Fatalf("%s Decrypt = %q, %v", name, got, err)
		}
	}
}

func TestSecretsDoNotDecryptWithAnotherIdentity(t *testing.T) {
	_, recipient, _ := GenerateIdentity()
	otherID, _, _ := GenerateIdentity()
	ciphertext, err := Encrypt([]byte("A=1\n"), []string{recipient})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decrypt(ciphertext, otherID); err == nil || !strings.Contains(err.Error(), "this server's identity") {
		t.Fatalf("Decrypt = %v, want a refusal", err)
	}
}

func TestEncryptNeedsValidRecipients(t *testing.T) {
	if _, err := Encrypt([]byte("A=1\n"), nil); err == nil {
		t.Fatal("Encrypt without recipients succeeded")
	}
	if _, err := Encrypt([]byte("A=1\n"), []string{"ssh-ed25519 AAAA"}); err == nil {
		t.Fatal("Encrypt to an SSH key succeeded; only age X25519 recipients are accepted")
	}
	_, recipient, _ := GenerateIdentity()
	if err := ParseRecipient(recipient); err != nil {
		t.Fatalf("ParseRecipient(%q) = %v", recipient, err)
	}
	if err := ParseRecipient("age1notakey"); err == nil {
		t.Fatal("ParseRecipient accepted a malformed key")
	}
}

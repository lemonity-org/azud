package release

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"filippo.io/age"
)

// MaxSecretsBytes caps the runtime secrets file a bundle carries, encrypted or
// decrypted.
const MaxSecretsBytes = 1 << 20

// ParseRecipient checks that s is an age X25519 recipient (age1...).
func ParseRecipient(s string) error {
	if _, err := age.ParseX25519Recipient(strings.TrimSpace(s)); err != nil {
		return fmt.Errorf("%q is not an age X25519 recipient: %w", s, err)
	}
	return nil
}

// Encrypt encrypts plaintext so that any one of the recipients can decrypt it.
func Encrypt(plaintext []byte, recipients []string) ([]byte, error) {
	if len(recipients) == 0 {
		return nil, fmt.Errorf("no recipients to encrypt the secrets to")
	}
	if len(plaintext) > MaxSecretsBytes {
		return nil, fmt.Errorf("secrets exceed %d bytes", MaxSecretsBytes)
	}
	parsed := make([]age.Recipient, 0, len(recipients))
	for _, r := range recipients {
		recipient, err := age.ParseX25519Recipient(strings.TrimSpace(r))
		if err != nil {
			return nil, fmt.Errorf("%q is not an age X25519 recipient: %w", r, err)
		}
		parsed = append(parsed, recipient)
	}

	var out bytes.Buffer
	w, err := age.Encrypt(&out, parsed...)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(plaintext); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// Decrypt decrypts ciphertext with an age identity file's contents (one
// AGE-SECRET-KEY-1... line; comment lines are ignored).
func Decrypt(ciphertext []byte, identityFile []byte) ([]byte, error) {
	identities, err := age.ParseIdentities(bytes.NewReader(identityFile))
	if err != nil {
		return nil, fmt.Errorf("unreadable age identity: %w", err)
	}
	r, err := age.Decrypt(bytes.NewReader(ciphertext), identities...)
	if err != nil {
		return nil, fmt.Errorf("cannot decrypt the secrets with this server's identity: %w", err)
	}
	plaintext, err := io.ReadAll(io.LimitReader(r, MaxSecretsBytes+1))
	if err != nil {
		return nil, fmt.Errorf("cannot decrypt the secrets: %w", err)
	}
	if len(plaintext) > MaxSecretsBytes {
		return nil, fmt.Errorf("decrypted secrets exceed %d bytes", MaxSecretsBytes)
	}
	return plaintext, nil
}

// GenerateIdentity creates an age X25519 identity. It returns the identity
// file contents, which stay on the server, and the public recipient.
func GenerateIdentity() (identityFile []byte, recipient string, err error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, "", err
	}
	recipient = id.Recipient().String()
	return []byte("# public key: " + recipient + "\n" + id.String() + "\n"), recipient, nil
}

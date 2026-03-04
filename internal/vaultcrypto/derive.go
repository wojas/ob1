package vaultcrypto

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
	"golang.org/x/crypto/scrypt"
	"golang.org/x/text/unicode/norm"
)

const keySize = 32

func DeriveKey(password string, salt string) ([]byte, error) {
	normalizedPassword := norm.NFKC.String(password)
	normalizedSalt := norm.NFKC.String(salt)

	key, err := scrypt.Key([]byte(normalizedPassword), []byte(normalizedSalt), 32768, 8, 1, keySize)
	if err != nil {
		return nil, fmt.Errorf("derive vault key: %w", err)
	}

	return key, nil
}

func KeyHash(rawKey []byte, salt string, encryptionVersion int) (string, error) {
	switch encryptionVersion {
	case 0:
		sum := sha256.Sum256(rawKey)
		return hex.EncodeToString(sum[:]), nil
	case 2, 3:
		normalizedSalt := norm.NFKC.String(salt)
		reader := hkdf.New(sha256.New, rawKey, []byte(normalizedSalt), []byte("ObsidianKeyHash"))
		derived := make([]byte, keySize)
		if _, err := io.ReadFull(reader, derived); err != nil {
			return "", fmt.Errorf("derive keyhash: %w", err)
		}
		return hex.EncodeToString(derived), nil
	default:
		return "", fmt.Errorf("unsupported encryption version %d", encryptionVersion)
	}
}

func EncodeKey(rawKey []byte) string {
	return base64.StdEncoding.EncodeToString(rawKey)
}

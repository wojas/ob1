package vaultcrypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

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

func DecodeKey(encoded string) ([]byte, error) {
	trimmed := strings.TrimSpace(encoded)
	if trimmed == "" {
		return nil, errors.New("missing encoded vault key")
	}

	key, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		return nil, fmt.Errorf("decode vault key: %w", err)
	}

	if len(key) != keySize {
		return nil, fmt.Errorf("decoded vault key has length %d, want %d", len(key), keySize)
	}

	return key, nil
}

func DecodeMetadata(rawKey []byte, salt string, encryptionVersion int, encoded string) (string, error) {
	switch encryptionVersion {
	case 0:
		return decodeMetadataV0(rawKey, encoded)
	case 2, 3:
		return decodeMetadataSIV(rawKey, salt, encoded)
	default:
		return "", fmt.Errorf("unsupported encryption version %d", encryptionVersion)
	}
}

func EncodeMetadata(rawKey []byte, salt string, encryptionVersion int, plaintext string) (string, error) {
	switch encryptionVersion {
	case 0:
		return encodeMetadataV0(rawKey, plaintext)
	case 2, 3:
		return encodeMetadataSIV(rawKey, salt, plaintext)
	default:
		return "", fmt.Errorf("unsupported encryption version %d", encryptionVersion)
	}
}

func DecodeFileBody(rawKey []byte, encryptionVersion int, body []byte) ([]byte, error) {
	if len(body) == 0 {
		return nil, nil
	}

	switch encryptionVersion {
	case 0:
		return decryptGCM(rawKey, body)
	case 2, 3:
		contentKey, err := hkdfBytes(rawKey, "", "ObsidianAesGcm", keySize)
		if err != nil {
			return nil, err
		}
		return decryptGCM(contentKey, body)
	default:
		return nil, fmt.Errorf("unsupported encryption version %d", encryptionVersion)
	}
}

func EncodeFileBody(rawKey []byte, encryptionVersion int, body []byte) ([]byte, error) {
	if len(body) == 0 {
		return nil, nil
	}

	switch encryptionVersion {
	case 0:
		return encryptGCM(rawKey, body)
	case 2, 3:
		contentKey, err := hkdfBytes(rawKey, "", "ObsidianAesGcm", keySize)
		if err != nil {
			return nil, err
		}
		return encryptGCM(contentKey, body)
	default:
		return nil, fmt.Errorf("unsupported encryption version %d", encryptionVersion)
	}
}

func PlaintextHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func encodeMetadataV0(rawKey []byte, plaintext string) (string, error) {
	plain := []byte(plaintext)
	sum := sha256.Sum256(plain)
	nonce := sum[:12]

	block, err := aes.NewCipher(rawKey)
	if err != nil {
		return "", fmt.Errorf("create AES cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create GCM: %w", err)
	}

	ciphertext := gcm.Seal(nil, nonce, plain, nil)
	out := append(append([]byte{}, nonce...), ciphertext...)
	return hex.EncodeToString(out), nil
}

func encodeMetadataSIV(rawKey []byte, salt string, plaintext string) (string, error) {
	encKey, err := hkdfBytes(rawKey, norm.NFKC.String(salt), "ObsidianAesSivEnc", keySize)
	if err != nil {
		return "", err
	}
	macKey, err := hkdfBytes(rawKey, norm.NFKC.String(salt), "ObsidianAesSivMac", keySize)
	if err != nil {
		return "", err
	}

	plain := []byte(plaintext)
	tag, err := s2v(macKey, plain)
	if err != nil {
		return "", err
	}

	iv := append([]byte(nil), tag...)
	iv[len(iv)-8] &= 0x7f
	iv[len(iv)-4] &= 0x7f

	block, err := aes.NewCipher(encKey)
	if err != nil {
		return "", fmt.Errorf("create AES cipher: %w", err)
	}

	ciphertext := make([]byte, len(plain))
	cipher.NewCTR(block, iv).XORKeyStream(ciphertext, plain)

	out := append(append([]byte{}, tag...), ciphertext...)
	return hex.EncodeToString(out), nil
}

func decodeMetadataV0(rawKey []byte, encoded string) (string, error) {
	body, err := hex.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("hex decode metadata: %w", err)
	}
	if len(body) < 12 {
		return "", errors.New("metadata ciphertext too short")
	}

	plaintext, err := decryptGCM(rawKey, body)
	if err != nil {
		return "", fmt.Errorf("decrypt metadata: %w", err)
	}

	return string(plaintext), nil
}

func decodeMetadataSIV(rawKey []byte, salt string, encoded string) (string, error) {
	body, err := hex.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("hex decode metadata: %w", err)
	}
	if len(body) < aes.BlockSize {
		return "", errors.New("metadata ciphertext too short")
	}

	encKey, err := hkdfBytes(rawKey, norm.NFKC.String(salt), "ObsidianAesSivEnc", keySize)
	if err != nil {
		return "", err
	}
	macKey, err := hkdfBytes(rawKey, norm.NFKC.String(salt), "ObsidianAesSivMac", keySize)
	if err != nil {
		return "", err
	}

	tag := append([]byte(nil), body[:aes.BlockSize]...)
	ciphertext := body[aes.BlockSize:]
	iv := append([]byte(nil), tag...)
	iv[len(iv)-8] &= 0x7f
	iv[len(iv)-4] &= 0x7f

	block, err := aes.NewCipher(encKey)
	if err != nil {
		return "", fmt.Errorf("create AES cipher: %w", err)
	}

	plaintext := make([]byte, len(ciphertext))
	cipher.NewCTR(block, iv).XORKeyStream(plaintext, ciphertext)

	expectedTag, err := s2v(macKey, plaintext)
	if err != nil {
		return "", err
	}
	if subtle.ConstantTimeCompare(tag, expectedTag) != 1 {
		return "", errors.New("metadata authentication failed")
	}

	return string(plaintext), nil
}

func hkdfBytes(ikm []byte, salt string, info string, size int) ([]byte, error) {
	reader := hkdf.New(sha256.New, ikm, []byte(salt), []byte(info))
	out := make([]byte, size)
	if _, err := io.ReadFull(reader, out); err != nil {
		return nil, fmt.Errorf("derive %s key: %w", info, err)
	}

	return out, nil
}

func s2v(macKey []byte, plaintext []byte) ([]byte, error) {
	d, err := aesCMAC(macKey, make([]byte, aes.BlockSize))
	if err != nil {
		return nil, err
	}

	var input []byte
	if len(plaintext) >= aes.BlockSize {
		input = append([]byte(nil), plaintext...)
		offset := len(input) - aes.BlockSize
		for i := 0; i < aes.BlockSize; i++ {
			input[offset+i] ^= d[i]
		}
	} else {
		input = dbl(d)
		padded := padBlock(plaintext)
		for i := 0; i < aes.BlockSize; i++ {
			input[i] ^= padded[i]
		}
	}

	return aesCMAC(macKey, input)
}

func aesCMAC(key []byte, message []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create CMAC cipher: %w", err)
	}

	l := make([]byte, aes.BlockSize)
	block.Encrypt(l, l)
	k1 := dbl(l)
	k2 := dbl(k1)

	n := 1
	if len(message) > 0 {
		n = (len(message) + aes.BlockSize - 1) / aes.BlockSize
	}

	lastComplete := len(message) > 0 && len(message)%aes.BlockSize == 0
	lastBlock := make([]byte, aes.BlockSize)
	if lastComplete {
		copy(lastBlock, message[(n-1)*aes.BlockSize:])
		for i := 0; i < aes.BlockSize; i++ {
			lastBlock[i] ^= k1[i]
		}
	} else {
		start := 0
		if len(message) > 0 {
			start = (n - 1) * aes.BlockSize
		}
		copy(lastBlock, padBlock(message[start:]))
		for i := 0; i < aes.BlockSize; i++ {
			lastBlock[i] ^= k2[i]
		}
	}

	x := make([]byte, aes.BlockSize)
	y := make([]byte, aes.BlockSize)
	for i := 0; i < n-1; i++ {
		copy(y, message[i*aes.BlockSize:(i+1)*aes.BlockSize])
		xorBlock(y, x)
		block.Encrypt(x, y)
	}

	copy(y, lastBlock)
	xorBlock(y, x)
	block.Encrypt(x, y)

	return append([]byte(nil), x...), nil
}

func dbl(block []byte) []byte {
	out := make([]byte, len(block))
	var carry byte
	for i := len(block) - 1; i >= 0; i-- {
		nextCarry := block[i] >> 7
		out[i] = (block[i] << 1) | carry
		carry = nextCarry
	}
	if carry != 0 {
		out[len(out)-1] ^= 0x87
	}
	return out
}

func padBlock(partial []byte) []byte {
	out := make([]byte, aes.BlockSize)
	copy(out, partial)
	if len(partial) < aes.BlockSize {
		out[len(partial)] = 0x80
	}
	return out
}

func xorBlock(dst []byte, src []byte) {
	for i := range dst {
		dst[i] ^= src[i]
	}
}

func decryptGCM(key []byte, body []byte) ([]byte, error) {
	if len(body) < 12 {
		return nil, errors.New("ciphertext too short")
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}

	plaintext, err := gcm.Open(nil, body[:12], body[12:], nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt GCM payload: %w", err)
	}

	return plaintext, nil
}

func encryptGCM(key []byte, body []byte) ([]byte, error) {
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("read random nonce: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}

	ciphertext := gcm.Seal(nil, nonce, body, nil)
	return append(nonce, ciphertext...), nil
}

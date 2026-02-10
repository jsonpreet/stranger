package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"regexp"
	"strings"
)

const (
	legacyVersionPrefix  = "v1:"
	currentVersionPrefix = "v2:"
	nonceSize            = 12
	keySize              = 32
	defaultLegacyKeyID   = "default"
)

var keyIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

type cipherEngine struct {
	aead cipher.AEAD
}

type Keyring struct {
	primaryKeyID string
	keys         map[string]*cipherEngine
	keyOrder     []string
}

type DecryptResult struct {
	Plaintext string
	KeyID     string
	Version   string
}

func NewKeyringFromConfig(keysSpec, primaryKeyID, legacyKey string) (*Keyring, error) {
	keyMaterials, keyOrder, err := parseKeySpec(keysSpec)
	if err != nil {
		return nil, err
	}

	legacyKey = strings.TrimSpace(legacyKey)
	if len(keyMaterials) == 0 {
		if legacyKey == "" {
			return nil, fmt.Errorf("configure STRANGER_SECRETS_KEYS or STRANGER_SECRETS_KEY")
		}
		keyMaterials = map[string]string{
			defaultLegacyKeyID: legacyKey,
		}
		keyOrder = []string{defaultLegacyKeyID}
	}

	if strings.TrimSpace(primaryKeyID) == "" {
		primaryKeyID = keyOrder[0]
	}
	if _, ok := keyMaterials[primaryKeyID]; !ok {
		return nil, fmt.Errorf("primary key id %q is not present in configured keys", primaryKeyID)
	}

	keys := make(map[string]*cipherEngine, len(keyMaterials))
	for _, keyID := range keyOrder {
		engine, err := newCipherEngine(keyMaterials[keyID])
		if err != nil {
			return nil, fmt.Errorf("invalid key %q: %w", keyID, err)
		}
		keys[keyID] = engine
	}

	return &Keyring{
		primaryKeyID: primaryKeyID,
		keys:         keys,
		keyOrder:     append([]string(nil), keyOrder...),
	}, nil
}

func (k *Keyring) PrimaryKeyID() string {
	return k.primaryKeyID
}

func (k *Keyring) Encrypt(plainText string) (string, error) {
	engine, ok := k.keys[k.primaryKeyID]
	if !ok {
		return "", fmt.Errorf("primary key %q is not loaded", k.primaryKeyID)
	}

	payload, err := engine.encryptRaw(plainText)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%s%s:%s", currentVersionPrefix, k.primaryKeyID, payload), nil
}

func (k *Keyring) Decrypt(cipherText string) (DecryptResult, error) {
	cipherText = strings.TrimSpace(cipherText)
	if strings.HasPrefix(cipherText, currentVersionPrefix) {
		return k.decryptV2(cipherText)
	}

	if strings.HasPrefix(cipherText, legacyVersionPrefix) {
		return k.decryptV1(cipherText)
	}

	return DecryptResult{}, fmt.Errorf("unsupported ciphertext format")
}

func (d DecryptResult) NeedsRotation(primaryKeyID string) bool {
	return d.Version != "v2" || d.KeyID != primaryKeyID
}

func (k *Keyring) decryptV2(cipherText string) (DecryptResult, error) {
	parts := strings.SplitN(strings.TrimPrefix(cipherText, currentVersionPrefix), ":", 2)
	if len(parts) != 2 {
		return DecryptResult{}, fmt.Errorf("invalid v2 ciphertext format")
	}

	keyID := strings.TrimSpace(parts[0])
	payload := strings.TrimSpace(parts[1])
	engine, ok := k.keys[keyID]
	if !ok {
		return DecryptResult{}, fmt.Errorf("unknown key id %q", keyID)
	}

	plainText, err := engine.decryptRaw(payload)
	if err != nil {
		return DecryptResult{}, err
	}

	return DecryptResult{
		Plaintext: plainText,
		KeyID:     keyID,
		Version:   "v2",
	}, nil
}

func (k *Keyring) decryptV1(cipherText string) (DecryptResult, error) {
	payload := strings.TrimPrefix(cipherText, legacyVersionPrefix)
	for _, keyID := range k.keyOrder {
		engine := k.keys[keyID]
		plainText, err := engine.decryptRaw(payload)
		if err == nil {
			return DecryptResult{
				Plaintext: plainText,
				KeyID:     keyID,
				Version:   "v1",
			}, nil
		}
	}
	return DecryptResult{}, fmt.Errorf("failed to decrypt v1 ciphertext with available keys")
}

func newCipherEngine(rawKey string) (*cipherEngine, error) {
	key, err := decodeKeyMaterial(rawKey)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create aes cipher: %w", err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create gcm cipher: %w", err)
	}

	return &cipherEngine{aead: aead}, nil
}

func (c *cipherEngine) encryptRaw(plainText string) (string, error) {
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("failed generating nonce: %w", err)
	}

	sealed := c.aead.Seal(nonce, nonce, []byte(plainText), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func (c *cipherEngine) decryptRaw(payload string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", fmt.Errorf("invalid ciphertext format: %w", err)
	}

	if len(raw) <= nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}

	nonce := raw[:nonceSize]
	message := raw[nonceSize:]

	plain, err := c.aead.Open(nil, nonce, message, nil)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt ciphertext: %w", err)
	}
	return string(plain), nil
}

func parseKeySpec(keysSpec string) (map[string]string, []string, error) {
	keysSpec = strings.TrimSpace(keysSpec)
	if keysSpec == "" {
		return map[string]string{}, []string{}, nil
	}

	entries := strings.Split(keysSpec, ",")
	keyMaterials := make(map[string]string, len(entries))
	keyOrder := make([]string, 0, len(entries))
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		pair := strings.SplitN(entry, "=", 2)
		if len(pair) != 2 {
			return nil, nil, fmt.Errorf("invalid key entry %q; expected key_id=key_material", entry)
		}

		keyID := strings.TrimSpace(pair[0])
		keyMaterial := strings.TrimSpace(pair[1])
		if !keyIDPattern.MatchString(keyID) {
			return nil, nil, fmt.Errorf("invalid key id %q", keyID)
		}
		if keyMaterial == "" {
			return nil, nil, fmt.Errorf("key material cannot be empty for key id %q", keyID)
		}
		if _, exists := keyMaterials[keyID]; exists {
			return nil, nil, fmt.Errorf("duplicate key id %q", keyID)
		}

		keyMaterials[keyID] = keyMaterial
		keyOrder = append(keyOrder, keyID)
	}

	if len(keyMaterials) == 0 {
		return nil, nil, fmt.Errorf("no valid key entries in STRANGER_SECRETS_KEYS")
	}

	return keyMaterials, keyOrder, nil
}

func decodeKeyMaterial(rawKey string) ([]byte, error) {
	trimmed := strings.TrimSpace(rawKey)
	if trimmed == "" {
		return nil, fmt.Errorf("missing secret key material")
	}

	if strings.HasPrefix(trimmed, "base64:") {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(trimmed, "base64:"))
		if err != nil {
			return nil, fmt.Errorf("invalid base64 key: %w", err)
		}
		if len(decoded) != keySize {
			return nil, fmt.Errorf("decoded key must be %d bytes", keySize)
		}
		return decoded, nil
	}

	if decoded, err := base64.StdEncoding.DecodeString(trimmed); err == nil && len(decoded) == keySize {
		return decoded, nil
	}

	if len(trimmed) == keySize {
		return []byte(trimmed), nil
	}

	return nil, fmt.Errorf("key material must be a 32-byte key or base64-encoded 32-byte key")
}

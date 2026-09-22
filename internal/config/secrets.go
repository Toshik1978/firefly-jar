package config

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Environment variables that supply a secret instead of its *_file. A non-empty env var wins.
const (
	envVarFirefly       = "FIREFLY_JAR_FIREFLY_TOKEN"
	envVarEnableBanking = "FIREFLY_JAR_ENABLEBANKING_PRIVATE_KEY"
	envVarTelegram      = "FIREFLY_JAR_TELEGRAM_TOKEN"
	envVarSMTP          = "FIREFLY_JAR_SMTP_PASSWORD"
)

// groupOtherPermMask selects the permission bits that let anyone but the owner see a secret file.
const groupOtherPermMask = 0o077

// PEM block types for the two RSA private key encodings contracts/config.md accepts.
const (
	pemTypePKCS1 = "RSA PRIVATE KEY"
	pemTypePKCS8 = "PRIVATE KEY"
)

// Secrets holds the secret values ValidateFor resolved for one command. A secret the command does
// not use is left empty (or nil) and was never read.
type Secrets struct {
	FireflyToken  string
	TelegramToken string
	SMTPPassword  string
	PrivateKey    *rsa.PrivateKey
}

// secretSource names the two places one secret may come from. Key is the config key of the file,
// used in errors and warnings instead of the value, which never appears in either.
type secretSource struct {
	key    string
	envVar string
	file   string
}

// resolveSecret returns the whitespace-trimmed secret from the env var or, failing that, the file.
// The warning is non-empty when the file is readable by group or others.
func resolveSecret(env func(string) string, src secretSource) (value, warning string, err error) {
	if v := strings.TrimSpace(env(src.envVar)); v != "" {
		return v, "", nil
	}

	if src.file == "" {
		return "", "", fmt.Errorf("%s: not set, and %s is empty", src.key, src.envVar)
	}

	path := filepath.Clean(src.file)

	info, err := os.Stat(path)
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", src.key, err)
	}

	if perm := info.Mode().Perm(); perm&groupOtherPermMask != 0 {
		warning = fmt.Sprintf(
			"%s: %s has group/other permission bits (%#o); restrict it to the owner with chmod 600",
			src.key, path, perm,
		)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", src.key, err)
	}

	value = strings.TrimSpace(string(data))
	if value == "" {
		return "", "", fmt.Errorf("%s: %s is empty", src.key, path)
	}

	return value, warning, nil
}

// parsePrivateKey parses an RSA private key from PEM text in PKCS#1 or PKCS#8 form. Its errors
// describe the encoding problem only and never echo the key material.
func parsePrivateKey(pemText string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, errors.New("no PEM block found")
	}

	switch block.Type {
	case pemTypePKCS1:
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse PKCS#1 key: %w", err)
		}

		return key, nil
	case pemTypePKCS8:
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse PKCS#8 key: %w", err)
		}

		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("PKCS#8 key is %T, not RSA", key)
		}

		return rsaKey, nil
	default:
		return nil, fmt.Errorf("unsupported PEM block %q (want %q or %q)", block.Type, pemTypePKCS1, pemTypePKCS8)
	}
}

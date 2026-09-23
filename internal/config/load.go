package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/goccy/go-yaml"
)

// DefaultPath is where the config lives when neither --config nor FIREFLY_JAR_CONFIG names one.
const DefaultPath = "/etc/firefly-jar/config.yaml"

// envConfigPath names the config file when the --config flag is empty.
const envConfigPath = "FIREFLY_JAR_CONFIG"

// ResolvePath picks the config file path: the --config flag value, then FIREFLY_JAR_CONFIG, then
// DefaultPath. A nil env reads the process environment.
func ResolvePath(flagValue string, env func(string) string) string {
	if flagValue != "" {
		return flagValue
	}

	if env == nil {
		env = os.Getenv
	}

	if p := env(envConfigPath); p != "" {
		return p
	}

	return DefaultPath
}

// Load reads the config at path, decodes it strictly (an unknown key fails with its line and
// column), applies defaults, resolves relative paths against the config file's directory and runs
// the structural validation. It reads no secret: ValidateFor does that per command, looking env-var
// secrets up through env (nil means the process environment). Load has no non-fatal findings of its
// own: the secret-file permission warnings come from ValidateFor, which is what reads those files.
func Load(path string, env func(string) string) (*Config, error) {
	if env == nil {
		env = os.Getenv
	}

	clean := filepath.Clean(path)

	data, err := os.ReadFile(clean)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := newDefaultConfig()
	if err = yaml.UnmarshalWithOptions(data, cfg, yaml.Strict()); err != nil {
		return nil, decodeError(clean, err)
	}

	cfg.env = env
	cfg.resolvePaths(filepath.Dir(clean))

	if err = cfg.validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", clean, err)
	}

	return cfg, nil
}

// resolvePaths makes every relative file path absolute against dir (the config file's directory),
// so the config means the same thing whatever directory cron starts in, and cleans every path.
func (c *Config) resolvePaths(dir string) {
	paths := []*string{&c.StateFile, &c.LogFile, &c.Firefly.TokenFile, &c.EnableBanking.PrivateKeyFile}
	if c.Notify.Telegram != nil {
		paths = append(paths, &c.Notify.Telegram.BotTokenFile)
	}

	if c.Notify.Email != nil {
		paths = append(paths, &c.Notify.Email.PasswordFile)
	}

	for _, p := range paths {
		*p = resolvePath(dir, *p)
	}
}

// resolvePath returns p cleaned, joined to dir first when it is relative. An empty p stays empty so
// a missing key is still reported as missing.
func resolvePath(dir, p string) string {
	switch {
	case p == "":
		return ""
	case filepath.IsAbs(p):
		return filepath.Clean(p)
	default:
		return filepath.Join(dir, p)
	}
}

// decodeError reports a YAML error as file:line:column plus the parser's message. The parser's own
// Error() appends the surrounding source lines, which could carry account identifiers to stderr, so
// only the message and position are kept.
func decodeError(path string, err error) error {
	var yamlErr yaml.Error
	if errors.As(err, &yamlErr) && yamlErr.GetToken() != nil {
		pos := yamlErr.GetToken().Position

		return fmt.Errorf("config %s:%d:%d: %s", path, pos.Line, pos.Column, yamlErr.GetMessage())
	}

	msg, _, _ := strings.Cut(err.Error(), "\n")

	return fmt.Errorf("config %s: %s", path, msg)
}

// Package config loads the YAML configuration strictly, applies defaults, validates its structure,
// and resolves secrets per command. Secrets never live in the YAML structs: they are read only by
// ValidateFor, only for the command that needs them, and returned in a separate Secrets value.
package config

import "time"

// Defaults applied before decoding, so an explicit zero in the file (valid for the tolerance and
// the consent warning) is kept rather than mistaken for "unset".
const (
	defaultWindowDays        = 30
	defaultDateToleranceDays = 3
	defaultConsentWarnDays   = 7
	defaultLogLevel          = "info"
	defaultPSUType           = "personal"
)

// Config is the root of the configuration file (contracts/config.md). Every relative path in it has
// already been resolved against the config file's directory by Load.
type Config struct {
	Timezone          string          `yaml:"timezone"`
	WindowDays        int             `yaml:"window_days"`
	DateToleranceDays int             `yaml:"date_tolerance_days"`
	ConsentWarnDays   int             `yaml:"consent_warn_days"`
	StateFile         string          `yaml:"state_file"`
	LogFile           string          `yaml:"log_file"`
	LogLevel          string          `yaml:"log_level"`
	Firefly           Firefly         `yaml:"firefly"`
	EnableBanking     EnableBanking   `yaml:"enablebanking"`
	Banks             map[string]Bank `yaml:"banks"`
	Accounts          []AccountRule   `yaml:"accounts"`
	Notify            Notify          `yaml:"notify"`

	// Location is Timezone loaded once by Load, so every caller computes "today" in the same zone.
	Location *time.Location `yaml:"-"`

	// env is the environment lookup handed to Load, kept so ValidateFor resolves env-var secrets
	// from the same source without touching the process environment in tests.
	env func(string) string
}

// Bank is one entry under banks:. Its map key is the name `auth <bank>` takes.
type Bank struct {
	Name    string `yaml:"name"`
	Country string `yaml:"country"`
	Display string `yaml:"display"`
}

// AccountRule is one override under accounts: (FR-015). It identifies a bank account by exactly one
// of Hash or IBAN and either maps it to a Firefly III account or excludes it.
type AccountRule struct {
	Bank             string `yaml:"bank"`
	Hash             string `yaml:"hash"`
	IBAN             string `yaml:"iban"`
	Currency         string `yaml:"currency"`
	FireflyAccountID string `yaml:"firefly_account_id"`
	Exclude          bool   `yaml:"exclude"`
}

// newDefaultConfig returns a Config holding every default, ready to be decoded over.
func newDefaultConfig() *Config {
	return &Config{
		WindowDays:        defaultWindowDays,
		DateToleranceDays: defaultDateToleranceDays,
		ConsentWarnDays:   defaultConsentWarnDays,
		LogLevel:          defaultLogLevel,
		EnableBanking:     EnableBanking{PSUType: defaultPSUType},
	}
}

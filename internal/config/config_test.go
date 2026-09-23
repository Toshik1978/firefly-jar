package config_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/config"
)

// noEnv is an env lookup that never has anything set, for tests that do not exercise env-var secret
// overrides.
func noEnv(string) string { return "" }

// envFunc turns a map into the env func Load and ResolvePath expect, without ever touching the host
// environment.
func envFunc(m map[string]string) func(string) string {
	return func(key string) string { return m[key] }
}

// copyFixtureDir copies testdata/config (yaml files plus the secrets/ directory) into a fresh
// t.TempDir(), so every test gets its own writable, chmod-able copy and the shared fixtures are
// never mutated.
func copyFixtureDir(t *testing.T) string {
	t.Helper()

	const src = "../../testdata/config"

	dst := t.TempDir()

	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return fmt.Errorf("relative path of %s: %w", path, err)
		}

		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}

		data, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}

		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		t.Fatalf("copy fixture dir: %v", err)
	}

	return dst
}

// writeYAML writes content to dir/name and returns the path, for test cases that need a config
// variant not present among the shared fixtures.
func writeYAML(t *testing.T, dir, name, content string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}

	return path
}

// containsSubstring reports whether any element of list contains sub.
func containsSubstring(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}

	return false
}

// minimalYAML mirrors testdata/config/minimal.yaml: required fields only, every default left unset.
func minimalYAML() string {
	return `timezone: Europe/Vilnius
state_file: state.json

firefly:
  url: https://firefly.example.com
  token_file: secrets/firefly.token

enablebanking:
  app_id: 00000000-0000-0000-0000-000000000000
  private_key_file: secrets/enablebanking.pem
  redirect_url: https://example.com/eb-callback

banks:
  swedbank: { name: Swedbank, country: LT }

notify:
  telegram:
    bot_token_file: secrets/telegram.token
    chat_ids: [123456789]
`
}

// rangeYAML is minimalYAML with window_days, date_tolerance_days and consent_warn_days made
// explicit, for the range-check table tests.
func rangeYAML(window, tolerance, consent int) string {
	return fmt.Sprintf(`timezone: Europe/Vilnius
window_days: %d
date_tolerance_days: %d
consent_warn_days: %d
state_file: state.json

firefly:
  url: https://firefly.example.com
  token_file: secrets/firefly.token

enablebanking:
  app_id: 00000000-0000-0000-0000-000000000000
  private_key_file: secrets/enablebanking.pem
  redirect_url: https://example.com/eb-callback

banks:
  swedbank: { name: Swedbank, country: LT }

notify:
  telegram:
    bot_token_file: secrets/telegram.token
    chat_ids: [123456789]
`, window, tolerance, consent)
}

// timezoneYAML is minimalYAML with an overridable timezone value.
func timezoneYAML(tz string) string {
	return fmt.Sprintf(`timezone: %s
state_file: state.json

firefly:
  url: https://firefly.example.com
  token_file: secrets/firefly.token

enablebanking:
  app_id: 00000000-0000-0000-0000-000000000000
  private_key_file: secrets/enablebanking.pem
  redirect_url: https://example.com/eb-callback

banks:
  swedbank: { name: Swedbank, country: LT }

notify:
  telegram:
    bot_token_file: secrets/telegram.token
    chat_ids: [123456789]
`, tz)
}

// urlYAML is minimalYAML with an overridable firefly.url value.
func urlYAML(url string) string {
	return fmt.Sprintf(`timezone: Europe/Vilnius
state_file: state.json

firefly:
  url: %s
  token_file: secrets/firefly.token

enablebanking:
  app_id: 00000000-0000-0000-0000-000000000000
  private_key_file: secrets/enablebanking.pem
  redirect_url: https://example.com/eb-callback

banks:
  swedbank: { name: Swedbank, country: LT }

notify:
  telegram:
    bot_token_file: secrets/telegram.token
    chat_ids: [123456789]
`, url)
}

// portYAML is minimalYAML with a notify.email block whose port is overridable.
func portYAML(port int) string {
	return fmt.Sprintf(`timezone: Europe/Vilnius
state_file: state.json

firefly:
  url: https://firefly.example.com
  token_file: secrets/firefly.token

enablebanking:
  app_id: 00000000-0000-0000-0000-000000000000
  private_key_file: secrets/enablebanking.pem
  redirect_url: https://example.com/eb-callback

banks:
  swedbank: { name: Swedbank, country: LT }

notify:
  email:
    host: smtp.example.com
    port: %d
    username: firefly-jar@example.com
    password_file: secrets/smtp.password
    from: firefly-jar@example.com
    to: [me@example.com]
`, port)
}

// accountRuleYAML is minimalYAML plus one accounts[] override entry, its fields set by the caller,
// for the override-rule table tests.
func accountRuleYAML(bank, fields string) string {
	return fmt.Sprintf(`timezone: Europe/Vilnius
state_file: state.json

firefly:
  url: https://firefly.example.com
  token_file: secrets/firefly.token

enablebanking:
  app_id: 00000000-0000-0000-0000-000000000000
  private_key_file: secrets/enablebanking.pem
  redirect_url: https://example.com/eb-callback

banks:
  swedbank: { name: Swedbank, country: LT }

accounts:
  - bank: %s
%s

notify:
  telegram:
    bot_token_file: secrets/telegram.token
    chat_ids: [123456789]
`, bank, fields)
}

// fullYAMLOpts parameterizes fullYAML's secret file paths and notifier shape, for the ValidateFor
// per-command tests. An empty EmailPasswordFile omits the notify.email block entirely.
type fullYAMLOpts struct {
	StateFile         string
	LogFile           string
	FireflyTokenFile  string
	PrivateKeyFile    string
	TelegramTokenFile string
	ChatIDs           string
	EmailPasswordFile string
}

// defaultFullYAMLOpts points every secret at the real testdata/config/secrets fixture and configures
// both notifiers with a recipient, matching testdata/config/valid.yaml's shape.
func defaultFullYAMLOpts() fullYAMLOpts {
	return fullYAMLOpts{
		StateFile:         "state.json",
		LogFile:           "firefly-jar.log",
		FireflyTokenFile:  "secrets/firefly.token",
		PrivateKeyFile:    "secrets/enablebanking.pem",
		TelegramTokenFile: "secrets/telegram.token",
		ChatIDs:           "[123456789]",
		EmailPasswordFile: "secrets/smtp.password",
	}
}

// fullYAML renders a config with every section ValidateFor cares about, built from o.
func fullYAML(o fullYAMLOpts) string {
	email := ""
	if o.EmailPasswordFile != "" {
		email = fmt.Sprintf(`  email:
    host: smtp.example.com
    port: 587
    username: firefly-jar@example.com
    password_file: %s
    from: firefly-jar@example.com
    to: [me@example.com]
`, o.EmailPasswordFile)
	}

	return fmt.Sprintf(`timezone: Europe/Vilnius
state_file: %s
log_file: %s

firefly:
  url: https://firefly.example.com
  token_file: %s

enablebanking:
  app_id: 00000000-0000-0000-0000-000000000000
  private_key_file: %s
  redirect_url: https://example.com/eb-callback

banks:
  swedbank: { name: Swedbank, country: LT }

notify:
  telegram:
    bot_token_file: %s
    chat_ids: %s
%s`, o.StateFile, o.LogFile, o.FireflyTokenFile, o.PrivateKeyFile, o.TelegramTokenFile, o.ChatIDs, email)
}

// LoadSuite covers config.Load: strict decoding, defaults, structural validation and relative-path
// resolution (T014, contracts/config.md).
type LoadSuite struct {
	suite.Suite
}

// requireLoadSucceeds loads path and requires it to succeed, returning the config.
func (s *LoadSuite) requireLoadSucceeds(path string) *config.Config {
	cfg, err := config.Load(path, noEnv)

	s.Require().NoError(err)

	return cfg
}

// requireLoadFails loads path and requires it to fail, returning the error.
func (s *LoadSuite) requireLoadFails(path string) error {
	cfg, err := config.Load(path, noEnv)

	s.Require().Error(err)
	s.Nil(cfg)

	return fmt.Errorf("load %s: %w", path, err)
}

func (s *LoadSuite) TestValidYAMLLoads() {
	dir := copyFixtureDir(s.T())

	cfg := s.requireLoadSucceeds(filepath.Join(dir, "valid.yaml"))

	s.Require().NotNil(cfg.Location)
	s.Equal("Europe/Vilnius", cfg.Location.String())
	s.Equal(30, cfg.WindowDays)
	s.Equal(3, cfg.DateToleranceDays)
	s.Equal(7, cfg.ConsentWarnDays)
	s.Equal("info", cfg.LogLevel)
	s.Equal("https://firefly.example.com/api/v1", cfg.Firefly.URL)
	s.Equal("00000000-0000-0000-0000-000000000000", cfg.EnableBanking.AppID)
	s.Equal("https://example.com/eb-callback", cfg.EnableBanking.RedirectURL)
	s.Equal("personal", cfg.EnableBanking.PSUType)
	s.Equal(config.Bank{Name: "Swedbank", Country: "LT"}, cfg.Banks["swedbank"])
	s.Equal(config.Bank{Name: "Revolut", Country: "LT", Display: "Revolut"}, cfg.Banks["revolut"])
	s.Require().Len(cfg.Accounts, 3)
	s.Equal("swedbank", cfg.Accounts[0].Bank)
	s.Equal("WwpbCiJhY2NvdW50Ii…", cfg.Accounts[0].Hash)
	s.Equal("43", cfg.Accounts[0].FireflyAccountID)
	s.Equal("revolut", cfg.Accounts[1].Bank)
	s.Equal("LT000000000000000001", cfg.Accounts[1].IBAN)
	s.Equal("USD", cfg.Accounts[1].Currency)
	s.Equal("21", cfg.Accounts[1].FireflyAccountID)
	s.True(cfg.Accounts[2].Exclude)
	s.Require().NotNil(cfg.Notify.Telegram)
	s.Equal([]int64{123456789, -1001234567890}, cfg.Notify.Telegram.ChatIDs)
	s.Require().NotNil(cfg.Notify.Email)
	s.Equal("smtp.example.com", cfg.Notify.Email.Host)
	s.Equal(587, cfg.Notify.Email.Port)
	s.Equal([]string{"me@example.com", "spouse@example.com"}, cfg.Notify.Email.To)
}

func (s *LoadSuite) TestMinimalYAMLLoadsWithDefaults() {
	dir := copyFixtureDir(s.T())

	cfg := s.requireLoadSucceeds(filepath.Join(dir, "minimal.yaml"))

	s.Equal(30, cfg.WindowDays)
	s.Equal(3, cfg.DateToleranceDays)
	s.Equal(7, cfg.ConsentWarnDays)
	s.Equal("info", cfg.LogLevel)
	s.Equal("personal", cfg.EnableBanking.PSUType)
}

func (s *LoadSuite) TestNotifySectionIsOptionalAtLoad() {
	dir := copyFixtureDir(s.T())
	withoutNotify, _, found := strings.Cut(minimalYAML(), "\nnotify:")
	s.Require().True(found)
	path := writeYAML(s.T(), dir, "no-notify.yaml", withoutNotify)

	cfg := s.requireLoadSucceeds(path)

	s.Nil(cfg.Notify.Telegram)
	s.Nil(cfg.Notify.Email)
}

func (s *LoadSuite) TestRelativePathsResolveAgainstConfigDir() {
	dir := copyFixtureDir(s.T())

	cfg := s.requireLoadSucceeds(filepath.Join(dir, "valid.yaml"))

	s.Equal(filepath.Join(dir, "state.json"), cfg.StateFile)
	s.Equal(filepath.Join(dir, "firefly-jar.log"), cfg.LogFile)
	s.Equal(filepath.Join(dir, "secrets", "firefly.token"), cfg.Firefly.TokenFile)
	s.Equal(filepath.Join(dir, "secrets", "enablebanking.pem"), cfg.EnableBanking.PrivateKeyFile)
	s.Equal(filepath.Join(dir, "secrets", "telegram.token"), cfg.Notify.Telegram.BotTokenFile)
	s.Equal(filepath.Join(dir, "secrets", "smtp.password"), cfg.Notify.Email.PasswordFile)
}

func (s *LoadSuite) TestAbsolutePathsAreNotRewritten() {
	dir := copyFixtureDir(s.T())
	content := strings.Replace(
		minimalYAML(), "state_file: state.json", "state_file: /var/lib/firefly-jar/state.json", 1,
	)
	path := writeYAML(s.T(), dir, "absolute.yaml", content)

	cfg := s.requireLoadSucceeds(path)

	s.Equal("/var/lib/firefly-jar/state.json", cfg.StateFile)
}

func (s *LoadSuite) TestUnknownKeyNamesTheKeyAndItsLine() {
	dir := copyFixtureDir(s.T())
	content := "bogus_setting: true\n" + minimalYAML()
	path := writeYAML(s.T(), dir, "unknown-key.yaml", content)

	err := s.requireLoadFails(path)

	s.Require().ErrorContains(err, "bogus_setting")
	s.ErrorContains(err, "1:1")
}

func (s *LoadSuite) TestWindowDaysRange() {
	cases := map[string]struct {
		days int
		ok   bool
	}{
		"zero is below the minimum": {days: 0, ok: false},
		"one is the minimum":        {days: 1, ok: true},
		"365 is the maximum":        {days: 365, ok: true},
		"366 is above the maximum":  {days: 366, ok: false},
	}

	for name, tc := range cases {
		s.Run(name, func() {
			dir := copyFixtureDir(s.T())
			path := writeYAML(s.T(), dir, "window.yaml", rangeYAML(tc.days, 3, 7))

			if tc.ok {
				s.requireLoadSucceeds(path)
			} else {
				s.requireLoadFails(path)
			}
		})
	}
}

// TestSeveralProblemsAreReportedOnOneLine pins contracts/cli.md's "one-line error on stderr": a config
// with several invalid fields names every one of them, joined by "; " on a single line, so cron's
// mail and a grep of the output see the whole failure at once.
func (s *LoadSuite) TestSeveralProblemsAreReportedOnOneLine() {
	dir := copyFixtureDir(s.T())
	path := writeYAML(s.T(), dir, "several.yaml", rangeYAML(0, 99, 7))

	err := s.requireLoadFails(path)

	s.NotContains(err.Error(), "\n")
	s.Contains(err.Error(), "window_days")
	s.Contains(err.Error(), "; date_tolerance_days")
}

func (s *LoadSuite) TestDateToleranceDaysRange() {
	cases := map[string]struct {
		days int
		ok   bool
	}{
		"negative is below the minimum": {days: -1, ok: false},
		"zero is the minimum":           {days: 0, ok: true},
		"14 is the maximum":             {days: 14, ok: true},
		"15 is above the maximum":       {days: 15, ok: false},
	}

	for name, tc := range cases {
		s.Run(name, func() {
			dir := copyFixtureDir(s.T())
			path := writeYAML(s.T(), dir, "tolerance.yaml", rangeYAML(30, tc.days, 7))

			if tc.ok {
				s.requireLoadSucceeds(path)
			} else {
				s.requireLoadFails(path)
			}
		})
	}
}

func (s *LoadSuite) TestConsentWarnDaysRange() {
	cases := map[string]struct {
		days int
		ok   bool
	}{
		"negative is below the minimum": {days: -1, ok: false},
		"zero is the minimum":           {days: 0, ok: true},
		"90 is the maximum":             {days: 90, ok: true},
		"91 is above the maximum":       {days: 91, ok: false},
	}

	for name, tc := range cases {
		s.Run(name, func() {
			dir := copyFixtureDir(s.T())
			path := writeYAML(s.T(), dir, "consent.yaml", rangeYAML(30, 3, tc.days))

			if tc.ok {
				s.requireLoadSucceeds(path)
			} else {
				s.requireLoadFails(path)
			}
		})
	}
}

func (s *LoadSuite) TestInvalidTimezoneFails() {
	dir := copyFixtureDir(s.T())
	path := writeYAML(s.T(), dir, "timezone.yaml", timezoneYAML("Not/AZone"))

	err := s.requireLoadFails(path)

	s.ErrorContains(err, "timezone")
}

func (s *LoadSuite) TestFireflyURLSchemeRules() {
	cases := map[string]struct {
		url string
		ok  bool
	}{
		"https on a public host passes":      {url: "https://firefly.example.com", ok: true},
		"http on localhost passes":           {url: "http://localhost:8080", ok: true},
		"http on loopback IP passes":         {url: "http://127.0.0.1:8080", ok: true},
		"http on private LAN 10.x passes":    {url: "http://10.1.2.3", ok: true},
		"http on private LAN 172.x passes":   {url: "http://172.20.0.5", ok: true},
		"http on private LAN 192.168 passes": {url: "http://192.168.1.1", ok: true},
		"http on a public domain fails":      {url: "http://firefly.example.com", ok: false},
		"http on a public IP fails":          {url: "http://8.8.8.8", ok: false},
		"http on IPv6 loopback passes":       {url: "http://[::1]:8080", ok: true},
		"http on private IPv6 passes":        {url: "http://[fd00::1]", ok: true},
		"http on a 10.x-prefixed hostname fails": {
			url: "http://10.0.0.1.evil.example.com", ok: false,
		},
		"http on a 192.168-prefixed hostname fails": {
			url: "http://192.168.evil.example.com", ok: false,
		},
	}

	for name, tc := range cases {
		s.Run(name, func() {
			dir := copyFixtureDir(s.T())
			path := writeYAML(s.T(), dir, "url.yaml", urlYAML(tc.url))

			if tc.ok {
				s.requireLoadSucceeds(path)
			} else {
				s.requireLoadFails(path)
			}
		})
	}
}

func (s *LoadSuite) TestFireflyURLSuffixNormalization() {
	cases := map[string]struct {
		url  string
		want string
	}{
		"/api suffix normalizes to /api/v1": {
			url: "https://firefly.example.com/api", want: "https://firefly.example.com/api/v1",
		},
		"/api/v1 suffix is left as the base": {
			url: "https://firefly.example.com/api/v1", want: "https://firefly.example.com/api/v1",
		},
		"bare host normalizes to /api/v1": {
			url: "https://firefly.example.com", want: "https://firefly.example.com/api/v1",
		},
		"bare host with trailing slash normalizes to /api/v1": {
			url: "https://firefly.example.com/", want: "https://firefly.example.com/api/v1",
		},
		"/api/ with trailing slash normalizes to /api/v1": {
			url: "https://firefly.example.com/api/", want: "https://firefly.example.com/api/v1",
		},
		"/api/v1/ with trailing slash normalizes to /api/v1": {
			url: "https://firefly.example.com/api/v1/", want: "https://firefly.example.com/api/v1",
		},
		"a reverse-proxy sub-path keeps its prefix": {
			url: "https://example.com/firefly", want: "https://example.com/firefly/api/v1",
		},
		"a sub-path with /api normalizes to /api/v1": {
			url: "https://example.com/firefly/api", want: "https://example.com/firefly/api/v1",
		},
		"a sub-path with /api/v1/ is left as the base": {
			url: "https://example.com/firefly/api/v1/", want: "https://example.com/firefly/api/v1",
		},
	}

	for name, tc := range cases {
		s.Run(name, func() {
			dir := copyFixtureDir(s.T())
			path := writeYAML(s.T(), dir, "url.yaml", urlYAML(tc.url))

			cfg := s.requireLoadSucceeds(path)

			s.Equal(tc.want, cfg.Firefly.URL)
		})
	}
}

func (s *LoadSuite) TestFireflyURLRejectsUnexpectedPaths() {
	cases := map[string]string{
		"another API version":              "https://firefly.example.com/api/v2",
		"another API version on sub-path":  "https://example.com/firefly/api/v2",
		"an endpoint path":                 "https://firefly.example.com/api/v1/accounts",
		"an api segment inside the prefix": "https://firefly.example.com/api/x/api/v1",
	}

	for name, url := range cases {
		s.Run(name, func() {
			dir := copyFixtureDir(s.T())
			path := writeYAML(s.T(), dir, "url.yaml", urlYAML(url))

			err := s.requireLoadFails(path)

			s.ErrorContains(err, "firefly.url")
		})
	}
}

func (s *LoadSuite) TestFireflyURLRejectsCredentialsAndQueryWithoutEchoingThem() {
	cases := map[string]struct {
		url    string
		secret string
	}{
		"credentials in userinfo": {url: "https://user:s3cr3t-pw@firefly.example.com", secret: "s3cr3t-pw"},
		"token in the query":      {url: "https://firefly.example.com/?token=q9secret", secret: "q9secret"},
	}

	for name, tc := range cases {
		s.Run(name, func() {
			dir := copyFixtureDir(s.T())
			path := writeYAML(s.T(), dir, "url.yaml", urlYAML(tc.url))

			err := s.requireLoadFails(path)

			s.Require().ErrorContains(err, "firefly.url")
			s.NotContains(err.Error(), tc.secret)
		})
	}
}

// exampleConfigPath is the repository's documented example config, relative to this package.
const exampleConfigPath = "../../config.example.yaml"

// TestRepositoryExampleConfigLoads loads the config.example.yaml the README tells the owner to copy,
// so the example cannot drift from what Load accepts: an unknown key, a renamed field or a value
// outside its range fails here rather than on the owner's first run.
func (s *LoadSuite) TestRepositoryExampleConfigLoads() {
	cfg := s.requireLoadSucceeds(exampleConfigPath)

	s.NotEmpty(cfg.Banks)
	s.NotEmpty(cfg.Accounts)
	s.Require().NotNil(cfg.Notify.Telegram)
	s.Require().NotNil(cfg.Notify.Email)

	for _, rule := range cfg.Accounts {
		s.Contains(cfg.Banks, rule.Bank, "every accounts: rule in the example names a bank the example defines")
	}
}

func (s *LoadSuite) TestLocalTimezoneIsRejected() {
	dir := copyFixtureDir(s.T())
	path := writeYAML(s.T(), dir, "timezone.yaml", timezoneYAML("Local"))

	err := s.requireLoadFails(path)

	s.ErrorContains(err, "timezone")
}

func (s *LoadSuite) TestFormatChecks() {
	cases := map[string]struct {
		old, replacement string
		key              string
	}{
		"non-numeric firefly_account_id fails": {
			old: "firefly_account_id: 43", replacement: "firefly_account_id: abc", key: "firefly_account_id",
		},
		"lower-case country fails": {
			old: "{ name: Swedbank, country: LT }", replacement: "{ name: Swedbank, country: lt }", key: "country",
		},
		"three-letter country fails": {
			old: "{ name: Swedbank, country: LT }", replacement: "{ name: Swedbank, country: LTU }", key: "country",
		},
		"lower-case currency fails": {
			old: "currency: USD", replacement: "currency: usd", key: "currency",
		},
		"two-letter currency fails": {
			old: "currency: USD", replacement: "currency: US", key: "currency",
		},
		"relative redirect_url fails": {
			old:         "redirect_url: https://example.com/eb-callback",
			replacement: "redirect_url: /eb-callback",
			key:         "redirect_url",
		},
		"empty app_id fails": {
			old:         "app_id: 00000000-0000-0000-0000-000000000000",
			replacement: `app_id: ""`,
			key:         "app_id",
		},
	}

	for name, tc := range cases {
		s.Run(name, func() {
			dir := copyFixtureDir(s.T())
			valid, err := os.ReadFile(filepath.Join(dir, "valid.yaml"))
			s.Require().NoError(err)
			s.Require().Contains(string(valid), tc.old)
			path := writeYAML(s.T(), dir, "format.yaml", strings.Replace(string(valid), tc.old, tc.replacement, 1))

			loadErr := s.requireLoadFails(path)

			s.ErrorContains(loadErr, tc.key)
		})
	}
}

func (s *LoadSuite) TestOverrideIBANIsNormalized() {
	dir := copyFixtureDir(s.T())
	fields := `    iban: lt00 0000 0000 0000 0001
    firefly_account_id: 1`
	path := writeYAML(s.T(), dir, "accounts.yaml", accountRuleYAML("swedbank", fields))

	cfg := s.requireLoadSucceeds(path)

	s.Require().Len(cfg.Accounts, 1)
	s.Equal("LT000000000000000001", cfg.Accounts[0].IBAN)
}

func (s *LoadSuite) TestWhitespaceOnlyIBANIsNotASelector() {
	dir := copyFixtureDir(s.T())
	fields := `    iban: "   "
    firefly_account_id: 1`
	path := writeYAML(s.T(), dir, "accounts.yaml", accountRuleYAML("swedbank", fields))

	err := s.requireLoadFails(path)

	s.ErrorContains(err, "hash or iban")
}

func (s *LoadSuite) TestEmailPortMustBe587Or465() {
	cases := map[string]struct {
		port int
		ok   bool
	}{
		"587 is STARTTLS":     {port: 587, ok: true},
		"465 is implicit TLS": {port: 465, ok: true},
		"25 is rejected":      {port: 25, ok: false},
		"2525 is rejected":    {port: 2525, ok: false},
	}

	for name, tc := range cases {
		s.Run(name, func() {
			dir := copyFixtureDir(s.T())
			path := writeYAML(s.T(), dir, "port.yaml", portYAML(tc.port))

			if tc.ok {
				s.requireLoadSucceeds(path)
			} else {
				s.requireLoadFails(path)
			}
		})
	}
}

func (s *LoadSuite) TestAccountRuleExactlyOneOfHashOrIBAN() {
	cases := map[string]string{
		"both hash and iban is invalid": `    hash: "abc123"
    iban: LT000000000000000001
    firefly_account_id: 1`,
		"neither hash nor iban is invalid": `    firefly_account_id: 1`,
	}

	for name, fields := range cases {
		s.Run(name, func() {
			dir := copyFixtureDir(s.T())
			path := writeYAML(s.T(), dir, "accounts.yaml", accountRuleYAML("swedbank", fields))

			s.requireLoadFails(path)
		})
	}
}

func (s *LoadSuite) TestAccountRuleExactlyOneOfFireflyAccountIDOrExclude() {
	cases := map[string]string{
		"both firefly_account_id and exclude is invalid": `    hash: "abc123"
    firefly_account_id: 1
    exclude: true`,
		"neither firefly_account_id nor exclude is invalid": `    hash: "abc123"`,
	}

	for name, fields := range cases {
		s.Run(name, func() {
			dir := copyFixtureDir(s.T())
			path := writeYAML(s.T(), dir, "accounts.yaml", accountRuleYAML("swedbank", fields))

			s.requireLoadFails(path)
		})
	}
}

func (s *LoadSuite) TestAccountRuleBankMustExistInBanks() {
	dir := copyFixtureDir(s.T())
	fields := `    hash: "abc123"
    firefly_account_id: 1`
	path := writeYAML(s.T(), dir, "accounts.yaml", accountRuleYAML("unknownbank", fields))

	err := s.requireLoadFails(path)

	s.ErrorContains(err, "unknownbank")
}

func (s *LoadSuite) TestResolvePath() {
	cases := map[string]struct {
		flag string
		env  map[string]string
		want string
	}{
		"the --config flag wins over everything": {
			flag: "/custom/config.yaml",
			env:  map[string]string{"FIREFLY_JAR_CONFIG": "/env/config.yaml"},
			want: "/custom/config.yaml",
		},
		"the env var wins when the flag is empty": {
			flag: "",
			env:  map[string]string{"FIREFLY_JAR_CONFIG": "/env/config.yaml"},
			want: "/env/config.yaml",
		},
		"the default is used when both are empty": {
			flag: "",
			env:  map[string]string{},
			want: "/etc/firefly-jar/config.yaml",
		},
	}

	for name, tc := range cases {
		s.Run(name, func() {
			got := config.ResolvePath(tc.flag, envFunc(tc.env))

			s.Equal(tc.want, got)
		})
	}
}

// ValidateSuite covers Config.ValidateFor: per-command secret resolution, permission warnings and
// the log_file/state_file writability checks (T014, contracts/config.md's per-command table).
type ValidateSuite struct {
	suite.Suite
}

// loadFull renders o as a config in dir, loads it with env and requires the load to succeed.
func (s *ValidateSuite) loadFull(dir string, o fullYAMLOpts, env func(string) string) *config.Config {
	path := writeYAML(s.T(), dir, "config.yaml", fullYAML(o))

	cfg, err := config.Load(path, env)

	s.Require().NoError(err)

	return cfg
}

func (s *ValidateSuite) TestAuthSucceedsWithFireflyTelegramSMTPFilesAllMissing() {
	dir := copyFixtureDir(s.T())
	o := defaultFullYAMLOpts()
	o.FireflyTokenFile = "secrets/does-not-exist-firefly.token"
	o.TelegramTokenFile = "secrets/does-not-exist-telegram.token"
	o.EmailPasswordFile = "secrets/does-not-exist-smtp.password"
	cfg := s.loadFull(dir, o, noEnv)

	secrets, warnings, err := cfg.ValidateFor(config.CmdAuth, false)

	s.Require().NoError(err)
	s.Empty(warnings)
	s.Require().NotNil(secrets.PrivateKey)
	s.Empty(secrets.FireflyToken)
	s.Empty(secrets.TelegramToken)
	s.Empty(secrets.SMTPPassword)
}

func (s *ValidateSuite) TestAccountsSucceedsWithNotifierSecretsMissing() {
	dir := copyFixtureDir(s.T())
	o := defaultFullYAMLOpts()
	o.TelegramTokenFile = "secrets/does-not-exist-telegram.token"
	o.EmailPasswordFile = "secrets/does-not-exist-smtp.password"
	cfg := s.loadFull(dir, o, noEnv)

	secrets, warnings, err := cfg.ValidateFor(config.CmdAccounts, false)

	s.Require().NoError(err)
	s.Empty(warnings)
	s.NotEmpty(secrets.FireflyToken)
	s.Require().NotNil(secrets.PrivateKey)
	s.Empty(secrets.TelegramToken)
	s.Empty(secrets.SMTPPassword)
}

func (s *ValidateSuite) TestAccountsFailsWithoutFireflyToken() {
	dir := copyFixtureDir(s.T())
	o := defaultFullYAMLOpts()
	o.FireflyTokenFile = "secrets/does-not-exist-firefly.token"
	cfg := s.loadFull(dir, o, noEnv)

	_, _, err := cfg.ValidateFor(config.CmdAccounts, false)

	s.Require().Error(err)
	s.ErrorContains(err, "firefly")
}

func (s *ValidateSuite) TestCheckStdoutDoesNotRequireNotifierSecretsOrRecipients() {
	dir := copyFixtureDir(s.T())
	o := defaultFullYAMLOpts()
	o.TelegramTokenFile = "secrets/does-not-exist-telegram.token"
	o.ChatIDs = "[]"
	o.EmailPasswordFile = ""
	cfg := s.loadFull(dir, o, noEnv)

	secrets, warnings, err := cfg.ValidateFor(config.CmdCheck, true)

	s.Require().NoError(err)
	s.Empty(warnings)
	s.Empty(secrets.TelegramToken)
	s.Empty(secrets.SMTPPassword)
}

func (s *ValidateSuite) TestCheckSucceedsWithNotifierSecretsAndRecipients() {
	dir := copyFixtureDir(s.T())
	cfg := s.loadFull(dir, defaultFullYAMLOpts(), noEnv)

	secrets, warnings, err := cfg.ValidateFor(config.CmdCheck, false)

	s.Require().NoError(err)
	s.Empty(warnings)
	s.Equal("telegram-test-token-0000000000000000", secrets.TelegramToken)
	s.Equal("smtp-test-password-0000000000000000", secrets.SMTPPassword)
}

func (s *ValidateSuite) TestCheckFailsWhenConfiguredNotifierSecretMissing() {
	dir := copyFixtureDir(s.T())
	o := defaultFullYAMLOpts()
	o.TelegramTokenFile = "secrets/does-not-exist-telegram.token"
	cfg := s.loadFull(dir, o, noEnv)

	_, _, err := cfg.ValidateFor(config.CmdCheck, false)

	s.Require().Error(err)
	s.ErrorContains(err, "telegram")
}

func (s *ValidateSuite) TestCheckFailsWithZeroRecipients() {
	dir := copyFixtureDir(s.T())
	o := defaultFullYAMLOpts()
	o.ChatIDs = "[]"
	o.EmailPasswordFile = ""
	cfg := s.loadFull(dir, o, noEnv)

	_, _, err := cfg.ValidateFor(config.CmdCheck, false)

	s.Require().Error(err)
	s.ErrorContains(err, "recipient")
}

func (s *ValidateSuite) TestLogFileDirCheckedOnlyForCheck() {
	dir := copyFixtureDir(s.T())
	o := defaultFullYAMLOpts()
	o.LogFile = "/nonexistent-dir-firefly-jar-test-xyz/firefly-jar.log"
	cfg := s.loadFull(dir, o, noEnv)

	_, _, errAuth := cfg.ValidateFor(config.CmdAuth, false)
	s.Require().NoError(errAuth)

	_, _, errAccounts := cfg.ValidateFor(config.CmdAccounts, false)
	s.Require().NoError(errAccounts)

	_, _, errCheck := cfg.ValidateFor(config.CmdCheck, false)
	s.Require().Error(errCheck)
	s.Require().ErrorContains(errCheck, "log")

	_, _, errCheckStdout := cfg.ValidateFor(config.CmdCheck, true)
	s.Require().Error(errCheckStdout)
	s.ErrorContains(errCheckStdout, "log")
}

// skipIfRoot skips permission-denial cases, which root bypasses.
func (s *ValidateSuite) skipIfRoot() {
	if os.Geteuid() == 0 {
		s.T().Skip("root bypasses file permission checks")
	}
}

func (s *ValidateSuite) TestAuthFailsWhenStateFileDirIsMissing() {
	dir := copyFixtureDir(s.T())
	o := defaultFullYAMLOpts()
	o.StateFile = "no-such-dir/state.json"
	cfg := s.loadFull(dir, o, noEnv)

	_, _, errAuth := cfg.ValidateFor(config.CmdAuth, false)
	s.Require().Error(errAuth)
	s.Require().ErrorContains(errAuth, "state_file")

	// For accounts and check a state file that does not exist yet is fine.
	_, _, errAccounts := cfg.ValidateFor(config.CmdAccounts, false)
	s.NoError(errAccounts)
}

func (s *ValidateSuite) TestAuthFailsWhenStateFileDirIsNotWritable() {
	s.skipIfRoot()

	dir := copyFixtureDir(s.T())
	stateDir := filepath.Join(dir, "state")
	s.Require().NoError(os.Mkdir(stateDir, 0o500))
	s.T().Cleanup(func() { _ = os.Chmod(stateDir, 0o700) })

	o := defaultFullYAMLOpts()
	o.StateFile = "state/state.json"
	cfg := s.loadFull(dir, o, noEnv)

	_, _, err := cfg.ValidateFor(config.CmdAuth, false)

	s.Require().Error(err)
	s.ErrorContains(err, "state_file")
}

func (s *ValidateSuite) TestStateFileMustBeReadableIfItExists() {
	cases := map[string]struct {
		cmd    config.Command
		stdout bool
	}{
		"accounts":       {cmd: config.CmdAccounts, stdout: false},
		"check --stdout": {cmd: config.CmdCheck, stdout: true},
		"check":          {cmd: config.CmdCheck, stdout: false},
	}

	for name, tc := range cases {
		s.Run(name+" accepts a readable state file", func() {
			dir := copyFixtureDir(s.T())
			s.Require().NoError(os.WriteFile(filepath.Join(dir, "state.json"), []byte("{}"), 0o600))
			cfg := s.loadFull(dir, defaultFullYAMLOpts(), noEnv)

			_, _, err := cfg.ValidateFor(tc.cmd, tc.stdout)

			s.NoError(err)
		})

		s.Run(name+" rejects a state_file that is a directory", func() {
			dir := copyFixtureDir(s.T())
			s.Require().NoError(os.Mkdir(filepath.Join(dir, "state.json"), 0o700))
			cfg := s.loadFull(dir, defaultFullYAMLOpts(), noEnv)

			_, _, err := cfg.ValidateFor(tc.cmd, tc.stdout)

			s.Require().Error(err)
			s.ErrorContains(err, "state_file")
		})

		s.Run(name+" rejects an unreadable state file", func() {
			s.skipIfRoot()

			dir := copyFixtureDir(s.T())
			s.Require().NoError(os.WriteFile(filepath.Join(dir, "state.json"), []byte("{}"), 0o000))
			cfg := s.loadFull(dir, defaultFullYAMLOpts(), noEnv)

			_, _, err := cfg.ValidateFor(tc.cmd, tc.stdout)

			s.Require().Error(err)
			s.ErrorContains(err, "state_file")
		})
	}
}

func (s *ValidateSuite) TestEnvVarWinsOverFileForFireflyTokenAndPrivateKey() {
	dir := copyFixtureDir(s.T())
	pemText, err := os.ReadFile(filepath.Clean(filepath.Join(dir, "secrets", "enablebanking.pem")))
	s.Require().NoError(err)

	o := defaultFullYAMLOpts()
	o.PrivateKeyFile = "secrets/does-not-exist.pem"
	env := envFunc(map[string]string{
		"FIREFLY_JAR_FIREFLY_TOKEN":             "env-firefly-token-xyz",
		"FIREFLY_JAR_ENABLEBANKING_PRIVATE_KEY": strings.TrimSpace(string(pemText)),
	})
	path := writeYAML(s.T(), dir, "config.yaml", fullYAML(o))
	cfg, loadErr := config.Load(path, env)
	s.Require().NoError(loadErr)

	secrets, warnings, valErr := cfg.ValidateFor(config.CmdAccounts, false)

	s.Require().NoError(valErr)
	s.Empty(warnings)
	s.Equal("env-firefly-token-xyz", secrets.FireflyToken)
	s.Require().NotNil(secrets.PrivateKey)
}

func (s *ValidateSuite) TestEnvVarWinsOverFileForTelegramTokenAndSMTPPassword() {
	dir := copyFixtureDir(s.T())
	o := defaultFullYAMLOpts()
	o.TelegramTokenFile = "secrets/does-not-exist-telegram.token"
	o.EmailPasswordFile = "secrets/does-not-exist-smtp.password"
	env := envFunc(map[string]string{
		"FIREFLY_JAR_TELEGRAM_TOKEN": "env-telegram-token-xyz",
		"FIREFLY_JAR_SMTP_PASSWORD":  "env-smtp-password-xyz",
	})
	path := writeYAML(s.T(), dir, "config.yaml", fullYAML(o))
	cfg, loadErr := config.Load(path, env)
	s.Require().NoError(loadErr)

	secrets, warnings, valErr := cfg.ValidateFor(config.CmdCheck, false)

	s.Require().NoError(valErr)
	s.Empty(warnings)
	s.Equal("env-telegram-token-xyz", secrets.TelegramToken)
	s.Equal("env-smtp-password-xyz", secrets.SMTPPassword)
}

func (s *ValidateSuite) TestSecretFileContentsAreWhitespaceTrimmed() {
	dir := copyFixtureDir(s.T())
	cfg := s.loadFull(dir, defaultFullYAMLOpts(), noEnv)

	secrets, _, err := cfg.ValidateFor(config.CmdAccounts, false)

	s.Require().NoError(err)
	s.Equal("firefly-test-token-0000000000000000", secrets.FireflyToken)
}

func (s *ValidateSuite) TestSecretFileGroupOrOtherPermissionsProduceWarningNotError() {
	dir := copyFixtureDir(s.T())
	tokenPath := filepath.Join(dir, "secrets", "firefly.token")
	s.Require().NoError(os.Chmod(tokenPath, 0o644))

	cfg := s.loadFull(dir, defaultFullYAMLOpts(), noEnv)

	secrets, warnings, err := cfg.ValidateFor(config.CmdAccounts, false)

	s.Require().NoError(err)
	s.NotEmpty(warnings)
	s.True(containsSubstring(warnings, "permission"), "warnings: %v", warnings)
	s.NotEmpty(secrets.FireflyToken)
}

func (s *ValidateSuite) TestPrivateKeyParsesFromPKCS8() {
	dir := copyFixtureDir(s.T())
	cfg := s.loadFull(dir, defaultFullYAMLOpts(), noEnv)

	secrets, _, err := cfg.ValidateFor(config.CmdAuth, false)

	s.Require().NoError(err)
	s.Require().NotNil(secrets.PrivateKey)
	s.NoError(secrets.PrivateKey.Validate())
}

func (s *ValidateSuite) TestPrivateKeyParsesFromPKCS1() {
	dir := copyFixtureDir(s.T())
	o := defaultFullYAMLOpts()
	o.PrivateKeyFile = "secrets/enablebanking_pkcs1.pem"
	cfg := s.loadFull(dir, o, noEnv)

	secrets, _, err := cfg.ValidateFor(config.CmdAuth, false)

	s.Require().NoError(err)
	s.Require().NotNil(secrets.PrivateKey)
	s.NoError(secrets.PrivateKey.Validate())
}

func (s *ValidateSuite) TestNonRSAPKCS8KeyIsRejected() {
	dir := copyFixtureDir(s.T())
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	s.Require().NoError(err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	s.Require().NoError(err)
	block := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	s.Require().NoError(os.WriteFile(filepath.Join(dir, "secrets", "ec.pem"), block, 0o600))

	o := defaultFullYAMLOpts()
	o.PrivateKeyFile = "secrets/ec.pem"
	cfg := s.loadFull(dir, o, noEnv)

	secrets, _, valErr := cfg.ValidateFor(config.CmdAuth, false)

	s.Require().Error(valErr)
	s.Require().ErrorContains(valErr, "not RSA")
	s.Nil(secrets.PrivateKey)
}

func (s *ValidateSuite) TestWhitespaceOnlyEnvVarFallsBackToFile() {
	dir := copyFixtureDir(s.T())
	env := envFunc(map[string]string{"FIREFLY_JAR_FIREFLY_TOKEN": "  \n\t "})
	cfg := s.loadFull(dir, defaultFullYAMLOpts(), env)

	secrets, _, err := cfg.ValidateFor(config.CmdAccounts, false)

	s.Require().NoError(err)
	s.Equal("firefly-test-token-0000000000000000", secrets.FireflyToken)
}

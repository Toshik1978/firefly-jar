package config

import (
	"crypto/rsa"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Command is a subcommand whose secrets and filesystem requirements ValidateFor checks.
type Command int

// The commands of contracts/cli.md. The zero value is deliberately not a command.
const (
	CmdAuth Command = iota + 1
	CmdAccounts
	CmdCheck
)

// Ranges and allowed values from contracts/config.md.
const (
	minWindowDays        = 1
	maxWindowDays        = 365
	minDateToleranceDays = 0
	maxDateToleranceDays = 14
	minConsentWarnDays   = 0
	maxConsentWarnDays   = 90
	smtpPortSTARTTLS     = 587
	smtpPortImplicitTLS  = 465
	countryCodeLength    = 2
	currencyCodeLength   = 3
	fireflyAPIPath       = "/api"
	fireflyAPIBasePath   = "/api/v1"
	localTimezone        = "Local"
)

// String returns the command's CLI name.
func (c Command) String() string {
	switch c {
	case CmdAuth:
		return "auth"
	case CmdAccounts:
		return "accounts"
	case CmdCheck:
		return "check"
	default:
		return fmt.Sprintf("command(%d)", int(c))
	}
}

// ValidateFor checks what cmd needs beyond the structure Load validated, following the per-command
// table of contracts/config.md, and resolves only the secrets cmd uses. Other secrets are never
// read. stdout matters only for CmdCheck: check --stdout needs no notifier secret or recipient.
// Every problem found is reported together; on error the returned Secrets is empty.
func (c *Config) ValidateFor(cmd Command, stdout bool) (Secrets, []string, error) {
	var (
		col     collector
		secrets Secrets
	)

	secrets.PrivateKey = c.resolvePrivateKey(&col)

	switch cmd {
	case CmdAuth:
		// State writes go through a temp file renamed into place, so the directory must be writable.
		col.fail(checkDirWritable("state_file", c.StateFile))
	case CmdAccounts:
		secrets.FireflyToken = col.secret(c.lookupEnv(), c.fireflyTokenSource())
		col.fail(checkReadableIfExists("state_file", c.StateFile))
	case CmdCheck:
		secrets.FireflyToken = col.secret(c.lookupEnv(), c.fireflyTokenSource())
		col.fail(checkReadableIfExists("state_file", c.StateFile))
		col.fail(c.checkLogFile())

		if !stdout {
			c.resolveNotifiers(&col, &secrets)
		}
	default:
		col.fail(fmt.Errorf("unknown command %d", int(cmd)))
	}

	if err := col.err(); err != nil {
		return Secrets{}, col.warnings, fmt.Errorf("config for %s: %w", cmd, err)
	}

	return secrets, col.warnings, nil
}

// validate runs the structural checks of contracts/config.md and fills the derived fields
// (Location, the normalized Firefly URL, normalized override IBANs).
func (c *Config) validate() error {
	var col collector

	c.validateGeneral(&col)
	c.validateFirefly(&col)
	c.validateEnableBanking(&col)
	c.validateBanks(&col)
	c.validateAccounts(&col)
	c.validateNotify(&col)

	return col.err()
}

func (c *Config) validateGeneral(col *collector) {
	switch loc, err := time.LoadLocation(c.Timezone); {
	case c.Timezone == "":
		col.fail(errors.New("timezone: required"))
	case c.Timezone == localTimezone:
		// time.LoadLocation maps "Local" to the host zone, which would make the window depend on
		// the server's settings instead of the config.
		col.fail(errors.New("timezone: want an IANA name such as Europe/Vilnius, not Local"))
	case err != nil:
		col.fail(fmt.Errorf("timezone: %w", err))
	default:
		c.Location = loc
	}

	col.fail(checkRange("window_days", c.WindowDays, minWindowDays, maxWindowDays))
	col.fail(checkRange("date_tolerance_days", c.DateToleranceDays, minDateToleranceDays, maxDateToleranceDays))
	col.fail(checkRange("consent_warn_days", c.ConsentWarnDays, minConsentWarnDays, maxConsentWarnDays))
	col.fail(checkRequired("state_file", c.StateFile))
	col.fail(checkOneOf("log_level", c.LogLevel, "debug", "info", "warn", "error"))
}

func (c *Config) validateFirefly(col *collector) {
	if c.Firefly.URL == "" {
		col.fail(errors.New("firefly.url: required"))

		return
	}

	normalized, err := normalizeFireflyURL(c.Firefly.URL)
	if err != nil {
		col.fail(fmt.Errorf("firefly.url: %w", err))

		return
	}

	c.Firefly.URL = normalized
}

func (c *Config) validateEnableBanking(col *collector) {
	col.fail(checkRequired("enablebanking.app_id", c.EnableBanking.AppID))
	col.fail(checkOneOf("enablebanking.psu_type", c.EnableBanking.PSUType, "personal", "business"))

	if c.EnableBanking.RedirectURL == "" {
		col.fail(errors.New("enablebanking.redirect_url: required"))

		return
	}

	if u, err := url.Parse(c.EnableBanking.RedirectURL); err != nil || u.Scheme == "" || u.Host == "" {
		col.fail(errors.New("enablebanking.redirect_url: not an absolute URL"))
	}
}

func (c *Config) validateBanks(col *collector) {
	if len(c.Banks) == 0 {
		col.fail(errors.New("banks: at least one bank is required"))

		return
	}

	for _, key := range slices.Sorted(maps.Keys(c.Banks)) {
		bank := c.Banks[key]
		col.fail(checkRequired("banks."+key+".name", bank.Name))

		if !isUpperLetters(bank.Country, countryCodeLength) {
			col.fail(fmt.Errorf("banks.%s.country: want a two-letter upper-case country code", key))
		}
	}
}

func (c *Config) validateAccounts(col *collector) {
	for i := range c.Accounts {
		rule := &c.Accounts[i]
		key := fmt.Sprintf("accounts[%d]", i)

		// Normalize before the selector check, so a whitespace-only value counts as unset. The IBAN
		// form matches the bank-side normalization (upper case, no spaces), so a grouped IBAN copied
		// from a statement still matches.
		rule.Hash = strings.TrimSpace(rule.Hash)
		rule.IBAN = strings.ToUpper(strings.Join(strings.Fields(rule.IBAN), ""))

		if _, ok := c.Banks[rule.Bank]; !ok {
			col.fail(fmt.Errorf("%s.bank: %q is not a key in banks", key, rule.Bank))
		}

		if (rule.Hash == "") == (rule.IBAN == "") {
			col.fail(fmt.Errorf("%s: set exactly one of hash or iban", key))
		}

		if (rule.FireflyAccountID != "") == rule.Exclude {
			col.fail(fmt.Errorf("%s: set exactly one of firefly_account_id or exclude: true", key))
		}

		if rule.FireflyAccountID != "" && !isDigits(rule.FireflyAccountID) {
			col.fail(fmt.Errorf("%s.firefly_account_id: want a numeric Firefly III account id", key))
		}

		if rule.Currency != "" && !isUpperLetters(rule.Currency, currencyCodeLength) {
			col.fail(fmt.Errorf("%s.currency: want a three-letter upper-case currency code", key))
		}
	}
}

func (c *Config) validateNotify(col *collector) {
	email := c.Notify.Email
	if email == nil {
		return
	}

	col.fail(checkRequired("notify.email.host", email.Host))
	col.fail(checkRequired("notify.email.username", email.Username))
	col.fail(checkRequired("notify.email.from", email.From))

	if email.Port != smtpPortSTARTTLS && email.Port != smtpPortImplicitTLS {
		col.fail(fmt.Errorf(
			"notify.email.port: %d is not allowed; use %d (STARTTLS) or %d (implicit TLS)",
			email.Port, smtpPortSTARTTLS, smtpPortImplicitTLS,
		))
	}
}

// lookupEnv returns the env lookup Load was given, or the process environment for a Config built
// by hand.
func (c *Config) lookupEnv() func(string) string {
	if c.env == nil {
		return os.Getenv
	}

	return c.env
}

func (c *Config) fireflyTokenSource() secretSource {
	return secretSource{key: "firefly.token_file", envVar: envVarFirefly, file: c.Firefly.TokenFile}
}

func (c *Config) resolvePrivateKey(col *collector) *rsa.PrivateKey {
	pemText := col.secret(c.lookupEnv(), secretSource{
		key: "enablebanking.private_key_file", envVar: envVarEnableBanking, file: c.EnableBanking.PrivateKeyFile,
	})
	if pemText == "" {
		return nil
	}

	key, err := parsePrivateKey(pemText)
	if err != nil {
		col.fail(fmt.Errorf("enablebanking private key: %w", err))

		return nil
	}

	return key
}

// resolveNotifiers resolves the secret of every configured notifier and requires at least one
// recipient across them.
func (c *Config) resolveNotifiers(col *collector, secrets *Secrets) {
	recipients := 0

	if tg := c.Notify.Telegram; tg != nil {
		secrets.TelegramToken = col.secret(c.lookupEnv(), secretSource{
			key: "notify.telegram.bot_token_file", envVar: envVarTelegram, file: tg.BotTokenFile,
		})
		recipients += len(tg.ChatIDs)
	}

	if email := c.Notify.Email; email != nil {
		secrets.SMTPPassword = col.secret(c.lookupEnv(), secretSource{
			key: "notify.email.password_file", envVar: envVarSMTP, file: email.PasswordFile,
		})
		recipients += len(email.To)
	}

	if recipients == 0 {
		col.fail(errors.New(
			"notify: check needs at least one recipient in notify.telegram.chat_ids or notify.email.to " +
				"(or run check --stdout)",
		))
	}
}

func (c *Config) checkLogFile() error {
	if c.LogFile == "" {
		return errors.New("log_file: required for check")
	}

	return checkDirWritable("log_file", c.LogFile)
}

// collector gathers every problem and warning of one validation pass, so the owner fixes the
// config in one edit instead of one error per run.
type collector struct {
	errs     []error
	warnings []string
}

func (col *collector) fail(err error) {
	if err != nil {
		col.errs = append(col.errs, err)
	}
}

func (col *collector) warn(warning string) {
	if warning != "" {
		col.warnings = append(col.warnings, warning)
	}
}

// secret resolves src, recording its warning and error, and returns the value (empty on error).
func (col *collector) secret(env func(string) string, src secretSource) string {
	value, warning, err := resolveSecret(env, src)
	col.warn(warning)
	col.fail(err)

	return value
}

func (col *collector) err() error {
	return errors.Join(col.errs...)
}

func checkRequired(key, value string) error {
	if value == "" {
		return fmt.Errorf("%s: required", key)
	}

	return nil
}

func checkRange(key string, value, low, high int) error {
	if value < low || value > high {
		return fmt.Errorf("%s: %d is outside %d..%d", key, value, low, high)
	}

	return nil
}

func checkOneOf(key, value string, allowed ...string) error {
	if !slices.Contains(allowed, value) {
		return fmt.Errorf("%s: %q is not one of %s", key, value, strings.Join(allowed, ", "))
	}

	return nil
}

// normalizeFireflyURL accepts https (or http for localhost and loopback/private-LAN IPs) and
// returns the API base ending in /api/v1, whether the owner wrote the host, …/api or …/api/v1
// (research R8). Its errors never echo the URL, which could carry credentials.
func normalizeFireflyURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", errors.New("not an absolute URL")
	}

	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("must not contain credentials, a query or a fragment")
	}

	if schemeErr := checkFireflyScheme(u); schemeErr != nil {
		return "", schemeErr
	}

	base, err := fireflyAPIBase(u.Path)
	if err != nil {
		return "", err
	}

	u.Path, u.RawPath = base, ""

	return u.String(), nil
}

// checkFireflyScheme allows plain http only where the token cannot cross a public network.
func checkFireflyScheme(u *url.URL) error {
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLANHost(u.Hostname()) {
			return nil
		}

		return errors.New("http is allowed only for localhost or a loopback/private-LAN IP; use https")
	default:
		return fmt.Errorf("scheme %q is not https", u.Scheme)
	}
}

// fireflyAPIBase strips a trailing /api or /api/v1 (optional trailing slash), keeps any remaining
// prefix, which is how Firefly III runs behind a reverse-proxy sub-path, and appends /api/v1. An api
// segment anywhere else (/api/v2, /api/v1/accounts) is rejected rather than appended to, so it never
// becomes a silently wrong base. The path is not echoed.
func fireflyAPIBase(path string) (string, error) {
	prefix := strings.TrimSuffix(path, "/")

	switch {
	case strings.HasSuffix(prefix, fireflyAPIBasePath):
		prefix = strings.TrimSuffix(prefix, fireflyAPIBasePath)
	case strings.HasSuffix(prefix, fireflyAPIPath):
		prefix = strings.TrimSuffix(prefix, fireflyAPIPath)
	}

	if slices.Contains(strings.Split(prefix, "/"), strings.TrimPrefix(fireflyAPIPath, "/")) {
		return "", errors.New("path may hold a proxy prefix but an api segment only as a trailing /api or /api/v1")
	}

	return prefix + fireflyAPIBasePath, nil
}

func isLANHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}

	addr, err := netip.ParseAddr(host)

	return err == nil && (addr.IsLoopback() || addr.IsPrivate())
}

func isUpperLetters(s string, length int) bool {
	if len(s) != length {
		return false
	}

	for _, r := range s {
		if r < 'A' || r > 'Z' {
			return false
		}
	}

	return true
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}

	return s != ""
}

// checkDirWritable proves the directory of file accepts new files by creating and removing a probe,
// which also covers read-only mounts and ACLs that mode bits alone would miss.
func checkDirWritable(key, file string) error {
	if file == "" {
		return fmt.Errorf("%s: required", key)
	}

	dir := filepath.Dir(filepath.Clean(file))

	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("%s: directory: %w", key, err)
	}

	if !info.IsDir() {
		return fmt.Errorf("%s: %s is not a directory", key, dir)
	}

	probe, err := os.CreateTemp(dir, ".firefly-jar-probe-*")
	if err != nil {
		return fmt.Errorf("%s: directory is not writable: %w", key, err)
	}

	if err = errors.Join(probe.Close(), os.Remove(probe.Name())); err != nil {
		return fmt.Errorf("%s: remove write probe: %w", key, err)
	}

	return nil
}

// checkReadableIfExists accepts a missing file (no state saved yet) but not one that exists and
// cannot be read.
func checkReadableIfExists(key, file string) error {
	f, err := os.Open(filepath.Clean(file))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}

	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("not a regular file")
	}

	if closeErr := f.Close(); err == nil {
		err = closeErr
	}

	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}

	return nil
}

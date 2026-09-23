package app_test

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Toshik1978/firefly-jar/internal/state"
)

// Fields every buildConfigYAML config shares, never varied by a case (contracts/config.md):
// window_days, date_tolerance_days and consent_warn_days always render 30/3/7, log_level always
// info, the Enable Banking app_id is always the anonymized all-zero UUID, and the redirect_url is
// always the anonymized example.com callback.
const (
	configWindowDays        = 30
	configDateToleranceDays = 3
	configConsentWarnDays   = 7
	configLogLevel          = "info"
	configAppID             = "00000000-0000-0000-0000-000000000000"
	configRedirectURL       = "https://example.com/eb-callback"
)

// configBank is one banks: entry buildConfigYAML renders. An empty display omits the display
// field, matching the shape a config without an explicit display name has (contracts/config.md:
// display is optional).
type configBank struct {
	key, name, country, display string
}

// configOpts is every variable part of the config-YAML shared across this package's suites
// (accounts_test.go, acceptance_us2_test.go, acceptance_us3_test.go, delivery_test.go,
// failfast_test.go): the fields every suite's own configYAML/ffConfigYAML method used to render by
// hand before this helper replaced them.
type configOpts struct {
	// extraTop are raw lines rendered before timezone:, one config key per line and without a
	// trailing newline (FailFastSuite's unknown-key case).
	extraTop []string

	timezone   string
	stateFile  string
	logFile    string
	fireflyURL string

	// fireflyTokenFile adds a firefly.token_file: line when set; left empty, no such line is
	// rendered and the token is expected to come from the environment instead, as most suites
	// already arrange (contracts/config.md: env wins over file).
	fireflyTokenFile string
	privateKeyFile   string

	banks []configBank

	// overrides are already-rendered accounts: rule lines (each ending in "\n"); a case builds its
	// own with fmt.Sprintf since the hash/iban/exclude shape varies too much per case to model here.
	// Left empty, no accounts: section is rendered at all.
	overrides []string

	// telegram and email gate their own notify: sub-block independently of whether their secret
	// file path is itself valid, so a case (FailFastSuite) can declare a channel while pointing it
	// at a broken file. Neither set renders no notify: section at all.
	telegram          bool
	telegramTokenFile string
	telegramChatIDs   []int64
	email             bool
	emailPasswordFile string
}

// buildConfigYAML renders one config.yaml for a package app_test suite from o. It is the one
// config-YAML renderer every suite in this package shares; a suite's own configYAML method (kept
// for readability at each call site) only assembles the configOpts and delegates here.
func buildConfigYAML(o configOpts) string {
	var b strings.Builder

	for _, line := range o.extraTop {
		fmt.Fprintf(&b, "%s\n", line)
	}

	fmt.Fprintf(&b, "timezone: %s\n", o.timezone)
	fmt.Fprintf(&b, "window_days: %d\n", configWindowDays)
	fmt.Fprintf(&b, "date_tolerance_days: %d\n", configDateToleranceDays)
	fmt.Fprintf(&b, "consent_warn_days: %d\n", configConsentWarnDays)
	fmt.Fprintf(&b, "state_file: %s\n", o.stateFile)
	fmt.Fprintf(&b, "log_file: %s\n", o.logFile)
	fmt.Fprintf(&b, "log_level: %s\n", configLogLevel)

	b.WriteString("firefly:\n")
	fmt.Fprintf(&b, "  url: %s\n", o.fireflyURL)

	if o.fireflyTokenFile != "" {
		fmt.Fprintf(&b, "  token_file: %s\n", o.fireflyTokenFile)
	}

	b.WriteString("enablebanking:\n")
	fmt.Fprintf(&b, "  app_id: %s\n", configAppID)
	fmt.Fprintf(&b, "  private_key_file: %s\n", o.privateKeyFile)
	fmt.Fprintf(&b, "  redirect_url: %s\n", configRedirectURL)

	b.WriteString("banks:\n")

	for _, bk := range o.banks {
		writeConfigBank(&b, bk)
	}

	if len(o.overrides) > 0 {
		b.WriteString("accounts:\n")

		for _, line := range o.overrides {
			b.WriteString(line)
		}
	}

	writeConfigNotify(&b, o)

	return b.String()
}

// writeConfigBank renders one banks: entry.
func writeConfigBank(b *strings.Builder, bk configBank) {
	fmt.Fprintf(b, "  %s: { name: %q, country: %s", bk.key, bk.name, bk.country)

	if bk.display != "" {
		fmt.Fprintf(b, ", display: %q", bk.display)
	}

	b.WriteString(" }\n")
}

// writeConfigNotify renders the notify: section when o declares at least one channel; a channel
// with an empty secret file path is still declared (so config validation still requires it), a
// case just breaks the path itself, never the declaration.
func writeConfigNotify(b *strings.Builder, o configOpts) {
	if !o.telegram && !o.email {
		return
	}

	b.WriteString("notify:\n")

	if o.telegram {
		b.WriteString("  telegram:\n")
		fmt.Fprintf(b, "    bot_token_file: %s\n", o.telegramTokenFile)
		fmt.Fprintf(b, "    chat_ids: [%s]\n", formatChatIDs(o.telegramChatIDs))
	}

	if o.email {
		b.WriteString("  email:\n")
		b.WriteString("    host: smtp.example.com\n")
		b.WriteString("    port: 587\n")
		b.WriteString("    username: firefly-jar@example.com\n")
		fmt.Fprintf(b, "    password_file: %s\n", o.emailPasswordFile)
		b.WriteString("    from: firefly-jar@example.com\n")
		b.WriteString("    to: [owner@example.com]\n")
	}
}

// formatChatIDs renders ids as the comma-separated list a chat_ids: [...] flow sequence holds.
func formatChatIDs(ids []int64) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}

	return strings.Join(parts, ", ")
}

// writeSecret writes content to path readable by the owner only (cliSecretFileMode), the mode
// every suite's secret fixtures use unless a case deliberately breaks it.
func writeSecret(t *testing.T, path, content string) {
	t.Helper()

	require.NoError(t, os.WriteFile(path, []byte(content), cliSecretFileMode))
}

// writeCheckBankState saves a state file at path carrying version and, when version equals
// state.CurrentVersion, one session for checkBankKey whose one account matches
// checkAccountsFile's Firefly III account #1 — the fixture DeliverySuite and FailFastSuite both
// build their config and bank-provider fixtures around.
func writeCheckBankState(t *testing.T, path string, version int) {
	t.Helper()

	st := &state.State{Version: version, Sessions: map[string]state.Session{}}

	if version == state.CurrentVersion {
		st.Sessions[checkBankKey] = state.Session{
			Provider:     "enablebanking",
			SessionID:    checkSessionID,
			ValidUntil:   time.Date(2027, 3, 20, 0, 0, 0, 0, time.UTC),
			AuthorizedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
			Accounts: []state.Account{{
				UID:      checkAccountUID,
				Hash:     checkAccountHash,
				IBAN:     checkAccountIBAN,
				Currency: "EUR",
				Name:     "Main",
			}},
		}
	}

	require.NoError(t, state.Save(path, st))
}

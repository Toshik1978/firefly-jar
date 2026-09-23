package enablebanking

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Toshik1978/firefly-jar/internal/bank"
	"github.com/Toshik1978/firefly-jar/internal/state"
)

// stateByteLength is how many random bytes StartAuth reads for its "state" value: hex-encoded, 16
// bytes become the 32 lowercase hex characters research R3 requires.
const stateByteLength = 16

// ASPSP is one bank Enable Banking exposes for a country and PSU type (research R3): the name and
// country FindASPSP matched on, and how long a consent for it may last.
type ASPSP struct {
	Name                   string
	Country                string
	MaximumConsentValidity time.Duration
}

// AuthStart is what StartAuth returns: the URL the owner opens in a browser, and the state
// ParseRedirect later checks the redirect against.
type AuthStart struct {
	URL   string
	State string
}

// aspspListResponse is the GET /aspsps response shape: only the fields FindASPSP reads.
type aspspListResponse struct {
	Aspsps []aspspEntry `json:"aspsps"`
}

// aspspEntry is one bank in an aspspListResponse.
type aspspEntry struct {
	Name                   string `json:"name"`
	Country                string `json:"country"`
	MaximumConsentValidity int64  `json:"maximum_consent_validity"`
}

// authRequest is the POST /auth body (research R3).
type authRequest struct {
	Access      authRequestAccess `json:"access"`
	Aspsp       authRequestAspsp  `json:"aspsp"`
	State       string            `json:"state"`
	RedirectURL string            `json:"redirect_url"`
	PsuType     string            `json:"psu_type"`
}

// authRequestAccess is the consent scope StartAuth always requests: transactions only, never
// balances (constitution §II asks for the minimum consent that still serves the reminder).
type authRequestAccess struct {
	ValidUntil   string `json:"valid_until"`
	Transactions bool   `json:"transactions"`
	Balances     bool   `json:"balances"`
}

// authRequestAspsp echoes back the bank StartAuth is starting a consent for.
type authRequestAspsp struct {
	Name    string `json:"name"`
	Country string `json:"country"`
}

// authStartResponse is the POST /auth response shape: only the URL StartAuth returns.
type authStartResponse struct {
	URL string `json:"url"`
}

// sessionRequest is the POST /sessions body: the authorization code ParseRedirect extracted.
type sessionRequest struct {
	Code string `json:"code"`
}

// sessionResponse is the POST /sessions response shape (research R3): only the fields CreateSession
// maps onto state.Session.
type sessionResponse struct {
	SessionID string           `json:"session_id"`
	Accounts  []sessionAccount `json:"accounts"`
	Access    sessionAccess    `json:"access"`
}

// sessionAccess carries the consent's expiry, the only field of "access" CreateSession reads.
type sessionAccess struct {
	ValidUntil time.Time `json:"valid_until"`
}

// sessionAccount is one account in a sessionResponse. IBAN may be absent (research R4).
type sessionAccount struct {
	AccountID          sessionAccountID `json:"account_id"`
	UID                string           `json:"uid"`
	IdentificationHash string           `json:"identification_hash"`
	Currency           string           `json:"currency"`
	Name               string           `json:"name"`
}

// sessionAccountID carries the account's IBAN, which the provider may omit (research R4).
type sessionAccountID struct {
	IBAN string `json:"iban"`
}

// FindASPSP looks up the bank named name for country and psuType (GET /aspsps?country=…&psu_type=
// …&service=AIS, research R3) and returns its exact entry. A name with no exact match fails,
// listing any bank whose name looks like a typo of it rather than guessing which one was meant.
func (c *Client) FindASPSP(ctx context.Context, name, country, psuType string) (ASPSP, error) {
	query := url.Values{
		"country":  {country},
		"psu_type": {psuType},
		"service":  {"AIS"},
	}

	var list aspspListResponse
	if err := c.doRequest(ctx, "/aspsps", query, &list); err != nil {
		return ASPSP{}, fmt.Errorf("find aspsp %s: %w", name, err)
	}

	for _, entry := range list.Aspsps {
		if entry.Name == name {
			return ASPSP{
				Name:                   entry.Name,
				Country:                entry.Country,
				MaximumConsentValidity: time.Duration(entry.MaximumConsentValidity) * time.Second,
			}, nil
		}
	}

	return ASPSP{}, fmt.Errorf("find aspsp %s: no exact match%s", name, closeNames(name, list.Aspsps))
}

// StartAuth starts a consent for a (POST /auth, research R3): the access window runs from now for
// a.MaximumConsentValidity minus a one-hour safety margin, transactions-only, and a fresh 32-lower-
// case-hex state that the returned AuthStart also carries, so the caller can check ParseRedirect's
// result against it later.
func (c *Client) StartAuth(
	ctx context.Context, a ASPSP, redirectURL, psuType string, now time.Time,
) (AuthStart, error) {
	authState, err := randomState()
	if err != nil {
		return AuthStart{}, fmt.Errorf("start auth: %w", err)
	}

	body := authRequest{
		Access: authRequestAccess{
			ValidUntil:   now.Add(a.MaximumConsentValidity - time.Hour).Format(time.RFC3339),
			Transactions: true,
			Balances:     false,
		},
		Aspsp:       authRequestAspsp{Name: a.Name, Country: a.Country},
		State:       authState,
		RedirectURL: redirectURL,
		PsuType:     psuType,
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return AuthStart{}, fmt.Errorf("start auth: marshal request: %w", err)
	}

	var resp authStartResponse
	if err := c.request(ctx, http.MethodPost, "/auth", nil, raw, &resp); err != nil {
		return AuthStart{}, fmt.Errorf("start auth: %w", err)
	}

	return AuthStart{URL: resp.URL, State: authState}, nil
}

// ParseRedirect reads the URL the owner pasted back after opening AuthStart.URL (research R3, whose
// surrounding whitespace is tolerated), fails with the provider's own description if it carries an
// "error" parameter, fails with bank.ErrStateMismatch if "state" does not equal expectedState, and
// otherwise returns "code".
func ParseRedirect(pasted, expectedState string) (string, error) {
	trimmed := strings.TrimSpace(pasted)

	redirectURL, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("parse redirect: %w", err)
	}

	query := redirectURL.Query()

	if code := query.Get("error"); code != "" {
		desc := query.Get("error_description")
		if desc == "" {
			desc = code
		}

		return "", fmt.Errorf("parse redirect: %s", desc)
	}

	if got := query.Get("state"); got != expectedState {
		return "", fmt.Errorf("parse redirect: %w", bank.ErrStateMismatch)
	}

	code := query.Get("code")
	if code == "" {
		return "", errors.New("parse redirect: missing code")
	}

	return code, nil
}

// CreateSession exchanges code for a session (POST /sessions, research R3), never retried since a
// second authorization code would be rejected. AuthorizedAt is stamped now, injected so the result
// stays deterministic in tests; the provider's own "authorized_at" is not in the response.
func (c *Client) CreateSession(ctx context.Context, code string, now time.Time) (state.Session, error) {
	raw, err := json.Marshal(sessionRequest{Code: code})
	if err != nil {
		return state.Session{}, fmt.Errorf("create session: marshal request: %w", err)
	}

	var resp sessionResponse
	if err := c.request(ctx, http.MethodPost, "/sessions", nil, raw, &resp); err != nil {
		return state.Session{}, fmt.Errorf("create session: %w", err)
	}

	accounts := make([]state.Account, 0, len(resp.Accounts))
	for _, a := range resp.Accounts {
		accounts = append(accounts, state.Account{
			UID:      a.UID,
			Hash:     a.IdentificationHash,
			IBAN:     normalizeIBAN(a.AccountID.IBAN),
			Currency: a.Currency,
			Name:     a.Name,
		})
	}

	return state.Session{
		Provider:     "enablebanking",
		SessionID:    resp.SessionID,
		ValidUntil:   resp.Access.ValidUntil,
		AuthorizedAt: now,
		Accounts:     accounts,
	}, nil
}

// DeleteSession ends session id (DELETE /sessions/{id}, research R3). A failure here is always
// best-effort at the call site: the new session is saved before this runs.
func (c *Client) DeleteSession(ctx context.Context, id string) error {
	path := "/sessions/" + url.PathEscape(id)
	if err := c.request(ctx, http.MethodDelete, path, nil, nil, nil); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}

	return nil
}

// randomState returns 32 lowercase hex characters from crypto/rand (research R3): unguessable, and
// unique enough per run to catch a stale or replayed redirect.
func randomState() (string, error) {
	buf := make([]byte, stateByteLength)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate state: %w", err)
	}

	return hex.EncodeToString(buf), nil
}

// closeNames lists, as a parenthesized suffix, every entry whose name looks like a typo of name: a
// case-insensitive substring match in either direction, which is enough to catch a truncated or
// misspelled bank name without guessing which exact bank was meant. Empty when nothing looks close.
func closeNames(name string, entries []aspspEntry) string {
	lower := strings.ToLower(name)

	var matches []string

	for _, entry := range entries {
		entryLower := strings.ToLower(entry.Name)
		if strings.Contains(entryLower, lower) || strings.Contains(lower, entryLower) {
			matches = append(matches, entry.Name)
		}
	}

	if len(matches) == 0 {
		return ""
	}

	return " (close matches: " + strings.Join(matches, ", ") + ")"
}

// normalizeIBAN uppercases iban and strips spaces (research R4), matching the form the Firefly
// adapter normalizes to (internal/firefly/accounts.go) so the two compare equal. An absent IBAN
// stays "".
func normalizeIBAN(iban string) string {
	return strings.ToUpper(strings.Join(strings.Fields(iban), ""))
}

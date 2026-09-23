package enablebanking

import (
	"context"
	"fmt"
	"time"

	"github.com/Toshik1978/firefly-jar/internal/bank"
	"github.com/Toshik1978/firefly-jar/internal/config"
	"github.com/Toshik1978/firefly-jar/internal/state"
)

// Begin starts a consent for b (research R3, T059 ruling): FindASPSP resolves b's exact bank entry
// for c's configured PSU type, then StartAuth requests it using c's configured redirect URL and PSU
// type (set by WithAuthConfig), anchored at the wall clock.
func (c *Client) Begin(ctx context.Context, b config.Bank) (bank.PendingConsent, error) {
	aspsp, err := c.FindASPSP(ctx, b.Name, b.Country, c.psuType)
	if err != nil {
		return bank.PendingConsent{}, fmt.Errorf("begin consent: %w", err)
	}

	start, err := c.StartAuth(ctx, aspsp, c.redirectURL, c.psuType, time.Now())
	if err != nil {
		return bank.PendingConsent{}, fmt.Errorf("begin consent: %w", err)
	}

	return bank.PendingConsent{URL: start.URL, State: start.State}, nil
}

// Complete exchanges the redirect the owner pasted back for a session (research R3, T059 ruling):
// ParseRedirect checks it against p.State and extracts the code, then CreateSession exchanges that
// code, anchored at the wall clock.
func (c *Client) Complete(
	ctx context.Context, _ config.Bank, p bank.PendingConsent, pastedRedirect string,
) (state.Session, error) {
	code, err := ParseRedirect(pastedRedirect, p.State)
	if err != nil {
		return state.Session{}, fmt.Errorf("complete consent: %w", err)
	}

	session, err := c.CreateSession(ctx, code, time.Now())
	if err != nil {
		return state.Session{}, fmt.Errorf("complete consent: %w", err)
	}

	return session, nil
}

// Revoke ends the consent identified by sessionID (T059 ruling: Revoke = DeleteSession).
func (c *Client) Revoke(ctx context.Context, sessionID string) error {
	if err := c.DeleteSession(ctx, sessionID); err != nil {
		return fmt.Errorf("revoke consent: %w", err)
	}

	return nil
}

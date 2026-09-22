package bank

import (
	"context"

	"github.com/Toshik1978/firefly-jar/internal/civil"
	"github.com/Toshik1978/firefly-jar/internal/config"
	"github.com/Toshik1978/firefly-jar/internal/state"
)

// Provider fetches an account's transactions dated on or after from, read-only (constitution §IV):
// account-information consent only, no payment endpoint. A bank-specific adapter package implements
// it; nothing in package bank itself does, which is why iface's unused check needs overriding here.
//
//nolint:iface // producer interface for adapters implemented outside this package (constitution §IV).
type Provider interface {
	Transactions(ctx context.Context, sessionID string, acc Account, from civil.Date) ([]Transaction, error)
}

// PendingConsent is a consent flow that Authorizer.Begin has started but not yet completed: URL is
// what the owner opens in a browser, and State is the value Complete checks the redirect against
// (data-model.md "Authorizer"). Named PendingConsent, not Pending, because Status already has a
// Pending value in this package and Go does not scope constants by type.
type PendingConsent struct {
	URL   string
	State string
}

// Authorizer drives one bank's consent lifecycle end to end, independent of which provider
// implements it (constitution §IV, data-model.md "Authorizer"). A bank-specific adapter package
// implements it and app/auth.go consumes it; neither lives in package bank, hence the override below.
//
//nolint:iface // producer interface for adapters implemented outside this package (constitution §IV).
type Authorizer interface {
	// Begin starts a consent flow for b and returns the URL the owner must open.
	Begin(ctx context.Context, b config.Bank) (PendingConsent, error)
	// Complete exchanges the redirect the owner pasted back for a session, after checking it
	// against the State p carries.
	Complete(ctx context.Context, b config.Bank, p PendingConsent, pastedRedirect string) (state.Session, error)
	// Revoke ends a consent early.
	Revoke(ctx context.Context, sessionID string) error
}

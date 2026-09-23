package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Toshik1978/firefly-jar/internal/civil"
	"github.com/Toshik1978/firefly-jar/internal/config"
	"github.com/Toshik1978/firefly-jar/internal/state"
)

// authTimeout caps one whole auth run, the owner's browser login included, so a hung provider or an
// abandoned prompt can never keep the process alive for good (R12).
const authTimeout = 10 * time.Minute

// errNoRedirect is a paste that carried nothing, so there is no redirect to complete the consent with.
var errNoRedirect = errors.New("no redirect address pasted")

// errNoAuthorizer is a Factory that built auth's dependencies without an Authorizer.
var errNoAuthorizer = errors.New("no authorizer built")

// runAuth connects bankKey (FR-018, FR-019, contracts/cli.md). The key is checked against config
// before the state is read or any client is built, so a typo never touches the network. The new
// session is saved before the bank's previous one is revoked, so a failure at any step leaves the
// old, still-working session in place; a failed revoke only costs a WARN, since the new session is
// already on disk.
func (c *cli) runAuth(ctx context.Context, bankKey string) int {
	in, warnings, err := c.validated(config.CmdAuth, false)
	if err != nil {
		return c.fail(ctx, warnings, err)
	}

	if _, ok := in.Config.Banks[bankKey]; !ok {
		return c.fail(ctx, warnings, fmt.Errorf("unknown bank %q: not a key under banks: in config", bankKey))
	}

	more, err := in.loadState()
	warnings = append(warnings, more...)

	if err != nil {
		return c.fail(ctx, warnings, err)
	}

	ctx, cancel := context.WithTimeout(ctx, authTimeout)
	defer cancel()

	deps, err := c.env.Factory(ctx, in)
	if err != nil {
		return c.fail(ctx, warnings, fmt.Errorf("build dependencies: %w", err))
	}

	if deps.Close != nil {
		defer deps.Close()
	}

	for _, w := range warnings {
		deps.Log.WarnContext(ctx, "configuration warning", "warning", w)
	}

	if err = c.authorize(ctx, deps, bankKey); err != nil {
		fmt.Fprintf(c.env.Stderr, "firefly-jar: auth %s: %s\n", bankKey, deps.Redactor.Scrub(err.Error()))

		return exitError
	}

	return exitOK
}

// authorize runs the consent flow against deps.Authorizer alone, so it never depends on which
// provider implements it. Nothing is written until Complete has returned a session, and the
// previous session of the bank is revoked only once the new one is saved.
func (c *cli) authorize(ctx context.Context, deps Deps, bankKey string) error {
	if deps.Authorizer == nil {
		return errNoAuthorizer
	}

	b := deps.Config.Banks[bankKey]

	pending, err := deps.Authorizer.Begin(ctx, b)
	if err != nil {
		return fmt.Errorf("start consent: %w", err)
	}

	// The prompt ends without a newline: the terminal's echo of the paste and its Enter complete it.
	fmt.Fprintf(c.env.Stdout, "Open this link and log in to %s (%s):\n  %s\n%s\n> ", b.Name, b.Country, pending.URL,
		"After login your browser is redirected. Paste the full address from the address bar:")

	pasted, err := readRedirect(c.env.Stdin)
	if err != nil {
		return err
	}

	session, err := deps.Authorizer.Complete(ctx, b, pending, pasted)
	if err != nil {
		return fmt.Errorf("complete consent: %w", err)
	}

	previous, hadPrevious := deps.State.Sessions[bankKey]

	deps.State.Put(bankKey, session)

	if err = state.Save(deps.Config.StateFile, deps.State); err != nil {
		return fmt.Errorf("save state: %w", err)
	}

	if hadPrevious && previous.SessionID != "" && previous.SessionID != session.SessionID {
		// The session id itself is never logged; the bank key is enough to find the stale consent.
		if err = deps.Authorizer.Revoke(ctx, previous.SessionID); err != nil {
			deps.Log.WarnContext(ctx, "previous session not revoked", "bank", bankKey, "error", err)
		}
	}

	fmt.Fprintf(c.env.Stdout, "Connected %s (%s): %s, consent valid until %s.\n", b.Name, b.Country,
		countAccounts(len(session.Accounts)), civil.DateOf(session.ValidUntil.In(deps.Config.Location)))

	return nil
}

// readRedirect reads the one line the owner pasted. A final line without a newline still counts,
// since a piped paste may end at EOF.
func readRedirect(r io.Reader) (string, error) {
	if r == nil {
		return "", errNoRedirect
	}

	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read redirect: %w", err)
	}

	line = strings.TrimSpace(line)
	if line == "" {
		return "", errNoRedirect
	}

	return line, nil
}

// countAccounts renders n with the right plural of "account".
func countAccounts(n int) string {
	if n == 1 {
		return "1 account"
	}

	return fmt.Sprintf("%d accounts", n)
}

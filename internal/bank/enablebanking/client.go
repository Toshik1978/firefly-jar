package enablebanking

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/Toshik1978/firefly-jar/internal/bank"
	"github.com/Toshik1978/firefly-jar/internal/redact"
)

// DefaultBaseURL is the production Enable Banking API root, which callers pass to New.
const DefaultBaseURL = "https://api.enablebanking.com"

// Response body caps: a success page is decoded through maxBodyBytes so a runaway response cannot
// exhaust memory, and an error body is only ever read up to maxErrorBodyBytes.
const (
	maxBodyBytes      = 32 << 20
	maxErrorBodyBytes = 64 << 10
)

// Client is a read-only Enable Banking API client (constitution §I): account information only,
// no payment endpoint, and no Psu-* header, because the tool runs unattended. redirectURL and
// psuType are unset until WithAuthConfig is called; Transactions never reads them, only Begin does.
type Client struct {
	baseURL  string
	http     *http.Client
	signer   *Signer
	redactor *redact.Redactor

	redirectURL string
	psuType     string
}

// New builds a Client for baseURL (normally DefaultBaseURL). The caller injects hc, which in
// production is httpx.NewClient over an httpx.RetryTransport, so retries and timeouts stay in
// httpx; signer mints the bearer JWT and r scrubs every provider message before it reaches an
// error.
func New(baseURL string, hc *http.Client, signer *Signer, r *redact.Redactor) *Client {
	return &Client{
		baseURL:  baseURL,
		http:     hc,
		signer:   signer,
		redactor: r,
	}
}

// WithAuthConfig returns a copy of c that carries redirectURL and psuType, the two Begin needs but
// the fixed bank.Authorizer signature has no room for (config.Bank names only the bank). It returns
// a copy, not a mutation, so the Provider a command already holds is never silently reconfigured
// underneath it; wire.go calls this once, when it builds the auth command's Authorizer.
func (c *Client) WithAuthConfig(redirectURL, psuType string) *Client {
	cp := *c
	cp.redirectURL = redirectURL
	cp.psuType = psuType

	return &cp
}

// doRequest sends one authenticated GET for path and query and decodes a 2xx JSON body into out.
// Every other outcome comes back as a mapped error (see errors.go) that never carries a raw body,
// a transaction description or the token.
func (c *Client) doRequest(ctx context.Context, path string, query url.Values, out any) error {
	return c.request(ctx, http.MethodGet, path, query, nil, out)
}

// request sends one authenticated method request to path with query and, when rawBody is non-nil,
// that body as a JSON payload. out is decoded from a 2xx response, or left untouched if nil (used
// by DeleteSession, whose 204 has no body). Every other outcome comes back as a mapped error (see
// errors.go) that never carries a raw body, a transaction description or the token. Split into
// buildRequest and sendRequest to keep each half's cyclomatic complexity low.
func (c *Client) request(
	ctx context.Context, method, path string, query url.Values, rawBody []byte, out any,
) error {
	req, err := c.buildRequest(ctx, method, path, query, rawBody)
	if err != nil {
		return err
	}

	return c.sendRequest(req, out)
}

// buildRequest assembles an authenticated request for method, path and query, with rawBody as its
// JSON payload when non-nil.
func (c *Client) buildRequest(
	ctx context.Context, method, path string, query url.Values, rawBody []byte,
) (*http.Request, error) {
	endpoint, err := url.JoinPath(c.baseURL, path)
	if err != nil {
		return nil, fmt.Errorf("build enable banking url: %w", err)
	}

	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	var body io.Reader = http.NoBody
	if rawBody != nil {
		body = bytes.NewReader(rawBody)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("build enable banking request: %w", err)
	}

	token, err := c.signer.Token()
	if err != nil {
		return nil, fmt.Errorf("authorize enable banking request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	if rawBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	return req, nil
}

// sendRequest issues req and decodes a 2xx JSON body into out, or maps any other outcome to a bank
// error (see errors.go). out may be nil when the caller expects no body (DeleteSession's 204).
func (c *Client) sendRequest(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return mapTransportError(err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))

		return c.mapErrorResponse(resp.StatusCode, errBody)
	}

	if out == nil {
		return nil
	}

	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(out); err != nil {
		return decodeError()
	}

	return nil
}

// decodeError reports an undecodable success body as incomplete bank data. The decoder's own
// message is dropped, since a custom decoder's error could echo body content.
func decodeError() error {
	return &bank.Error{Kind: bank.ErrDataIncomplete, Detail: "undecodable response body"}
}

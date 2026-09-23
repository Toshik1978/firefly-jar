package enablebanking

import (
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

// Client is a read-only Enable Banking API client (constitution §II): account information only,
// no payment endpoint, and no Psu-* header, because the tool runs unattended.
type Client struct {
	baseURL  string
	http     *http.Client
	signer   *Signer
	redactor *redact.Redactor
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

// doRequest sends one authenticated GET for path and query and decodes a 2xx JSON body into out.
// Every other outcome comes back as a mapped error (see errors.go) that never carries a raw body,
// a transaction description or the token.
func (c *Client) doRequest(ctx context.Context, path string, query url.Values, out any) error {
	endpoint, err := url.JoinPath(c.baseURL, path)
	if err != nil {
		return fmt.Errorf("build enable banking url: %w", err)
	}

	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return fmt.Errorf("build enable banking request: %w", err)
	}

	token, err := c.signer.Token()
	if err != nil {
		return fmt.Errorf("authorize enable banking request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return mapTransportError(err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))

		return c.mapErrorResponse(resp.StatusCode, body)
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

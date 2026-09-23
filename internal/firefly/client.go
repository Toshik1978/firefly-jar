package firefly

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Toshik1978/firefly-jar/internal/httpx"
)

// maxPages is the per-list safety cap on pages fetched (research R8): hitting it reports
// ErrDataIncomplete, never a silent truncation, so a server that keeps reporting more pages cannot
// keep the run going forever.
const maxPages = 200

// maxPageBytes bounds how much of one response body is decoded. A 500-row page is well under a
// megabyte; the bound only stops a misbehaving server from exhausting memory.
const maxPageBytes = 32 << 20

// Client is a read-only Firefly III API client (FR-001, constitution §I): its transport chain
// rejects every method but GET before a request leaves the process, so no caller mistake
// can turn a full-access personal token into a write against Firefly III.
type Client struct {
	baseURL string
	token   string
	hc      *http.Client
	logger  *slog.Logger
}

// Option configures a Client built by New. Options only set up the client; none can widen what
// it sends, which stays GET only.
type Option func(*Client)

// New builds a Client for baseURL, authenticating every request with token, trimmed of
// surrounding whitespace and newlines (a token loaded from a file commonly ends with one). base is
// the transport New's retry and read-only layers wrap; a nil base uses http.DefaultTransport. The
// composed chain is ReadOnlyTransport wrapping httpx.RetryTransport wrapping base, inside
// httpx.NewClient, so a write request never leaves the process and a redirect is never followed
// (research R8, R12). The client logs nothing unless WithLogger is passed.
func New(baseURL, token string, base http.RoundTripper, opts ...Option) *Client {
	if base == nil {
		base = http.DefaultTransport
	}

	c := &Client{
		baseURL: baseURL,
		token:   strings.TrimSpace(token),
		hc: httpx.NewClient(&ReadOnlyTransport{
			Base: &httpx.RetryTransport{Base: base},
		}),
		logger: slog.New(slog.DiscardHandler),
	}

	for _, opt := range opts {
		opt(c)
	}

	return c
}

// WithLogger makes the client log to logger: today only the DEBUG record for a split skipped as
// not comparable in the account's currency (research R8), which explains why a Firefly III entry
// the owner did enter was not matched. A nil logger keeps the default, which discards everything.
func WithLogger(logger *slog.Logger) Option {
	return func(c *Client) {
		if logger != nil {
			c.logger = logger
		}
	}
}

// get sends an authenticated GET for path and query q, and returns the raw response for a caller
// to decode. It sends Authorization: Bearer <token> and Accept: application/json (research R8). A
// non-2xx status -- including a redirect, which is never followed (R8: "a 3xx is treated as a
// Firefly error") -- is reported by statusError, naming only the status code and never the token,
// the query or the body, and the response body is closed before returning.
func (c *Client) get(ctx context.Context, path string, q url.Values) (*http.Response, error) {
	endpoint, err := url.JoinPath(c.baseURL, path)
	if err != nil {
		return nil, fmt.Errorf("firefly: build request url: %w", err)
	}

	if len(q) > 0 {
		endpoint += "?" + q.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("firefly: build request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("firefly: request: %w", err)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_ = resp.Body.Close()

		return nil, statusError(resp.StatusCode)
	}

	return resp, nil
}

// getPage fetches one list page and decodes it into dst, always closing the body.
func (c *Client) getPage(ctx context.Context, path string, q url.Values, dst any) error {
	resp, err := c.get(ctx, path, q)
	if err != nil {
		return err
	}

	defer func() { _ = resp.Body.Close() }()

	err = json.NewDecoder(io.LimitReader(resp.Body, maxPageBytes)).Decode(dst)
	if err != nil {
		return fmt.Errorf("firefly: decode page: %w", err)
	}

	return nil
}

// fetchPages walks every page of the list endpoint at path, calling visit with each page's
// resources in order. It drives the page parameter itself from 1 to meta.pagination.total_pages,
// never trusting current_page to advance; a missing meta.pagination means a single page. After
// maxPages pages with more still to come it returns ErrDataIncomplete (research R8). q is not
// modified.
func fetchPages[T any](ctx context.Context, c *Client, path string, q url.Values, visit func([]T)) error {
	query := maps.Clone(q)
	if query == nil {
		query = url.Values{}
	}

	for page := 1; ; page++ {
		query.Set("page", strconv.Itoa(page))

		var body listPage[T]

		err := c.getPage(ctx, path, query, &body)
		if err != nil {
			return fmt.Errorf("page %d: %w", page, err)
		}

		visit(body.Data)

		if body.Meta == nil || body.Meta.Pagination == nil || page >= body.Meta.Pagination.TotalPages {
			return nil
		}

		if page >= maxPages {
			return fmt.Errorf("%w: more than %d pages", ErrDataIncomplete, maxPages)
		}
	}
}

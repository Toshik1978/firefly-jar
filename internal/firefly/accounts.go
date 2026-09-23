package firefly

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// ListAccounts returns every Firefly III asset account, credit cards modelled as ccAsset
// included, across all pages of GET /accounts?type=asset&limit=500 (research R8). Liability
// accounts are not mapping targets and are not requested (R8a).
func (c *Client) ListAccounts(ctx context.Context) ([]Account, error) {
	q := url.Values{"type": {"asset"}, "limit": {"500"}}

	var accounts []Account

	err := fetchPages(ctx, c, "accounts", q, func(page []accountResource) {
		for i := range page {
			accounts = append(accounts, toAccount(&page[i]))
		}
	})
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}

	return accounts, nil
}

// toAccount maps one decoded account resource onto Account. The IBAN is normalized to uppercase
// with all whitespace removed so it compares equal to the bank's form; a null IBAN stays "". A
// missing active key counts as active (R8).
func toAccount(res *accountResource) Account {
	attrs := &res.Attributes

	active := true
	if attrs.Active != nil {
		active = *attrs.Active
	}

	return Account{
		ID:            res.ID,
		Name:          attrs.Name,
		IBAN:          strings.ToUpper(strings.Join(strings.Fields(attrs.IBAN), "")),
		Currency:      attrs.CurrencyCode,
		Role:          attrs.AccountRole,
		DecimalPlaces: attrs.CurrencyDecimalPlaces,
		Active:        active,
	}
}

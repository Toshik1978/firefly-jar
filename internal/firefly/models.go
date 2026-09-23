package firefly

// The structs below are hand-written models of the Firefly III JSON:API responses, covering only
// the fields this client reads (research R8). Unknown fields are ignored, and a JSON null leaves a
// plain string field empty, which is exactly the "null means none" rule R8 wants for iban,
// group_title and foreign_currency_code.

// listPage is one page of any Firefly III list endpoint: the resources plus the optional
// pagination block. Meta is a pointer so a missing meta key (a single page) is distinguishable
// from a present one.
type listPage[T any] struct {
	Data []T       `json:"data"`
	Meta *pageMeta `json:"meta"`
}

// pageMeta holds a list page's meta block. Pagination is a pointer for the same reason as
// listPage.Meta.
type pageMeta struct {
	Pagination *pagination `json:"pagination"`
}

// pagination carries only total_pages: the client counts pages itself and never trusts
// current_page to advance (research R8).
type pagination struct {
	TotalPages int `json:"total_pages"`
}

// accountResource is one entry of GET /accounts.
type accountResource struct {
	ID         string            `json:"id"`
	Attributes accountAttributes `json:"attributes"`
}

// accountAttributes are the account fields ListAccounts maps. Active is a pointer because a
// missing key counts as active, while an explicit false does not.
type accountAttributes struct {
	Active                *bool  `json:"active"`
	Name                  string `json:"name"`
	AccountRole           string `json:"account_role"`
	CurrencyCode          string `json:"currency_code"`
	IBAN                  string `json:"iban"`
	CurrencyDecimalPlaces uint8  `json:"currency_decimal_places"`
}

// groupResource is one transaction group of GET /accounts/{id}/transactions. The server
// paginates split rows, not groups, so the same group id can appear on consecutive pages
// carrying different splits.
type groupResource struct {
	ID         string          `json:"id"`
	Attributes groupAttributes `json:"attributes"`
}

// groupAttributes holds the group title and the splits of the group that touch the account.
type groupAttributes struct {
	GroupTitle   string      `json:"group_title"`
	Transactions []splitJSON `json:"transactions"`
}

// splitJSON is one split (transaction journal). Type is a plain string so a type this client does
// not know, such as "liability credit", decodes instead of failing the whole page; it is filtered
// out afterwards. Amounts stay strings until they are parsed exactly into a money.Amount.
type splitJSON struct {
	JournalID           string `json:"transaction_journal_id"`
	Type                string `json:"type"`
	Date                string `json:"date"`
	CurrencyCode        string `json:"currency_code"`
	ForeignCurrencyCode string `json:"foreign_currency_code"`
	Amount              string `json:"amount"`
	ForeignAmount       string `json:"foreign_amount"`
	Description         string `json:"description"`
	SourceID            string `json:"source_id"`
	DestinationID       string `json:"destination_id"`
}

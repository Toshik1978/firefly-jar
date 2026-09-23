package telegram_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf16"

	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/digest"
	"github.com/Toshik1978/firefly-jar/internal/notify"
	"github.com/Toshik1978/firefly-jar/internal/notify/telegram"
)

const (
	// testToken is shaped like a real bot token so the secrecy cases catch it leaking through a
	// URL, and testTokenSecret is its secret half, which must not leak on its own either.
	testToken       = "123456:ABC-secret"
	testTokenSecret = "ABC-secret"

	// chatA and chatB are synthetic chat ids: a group-style negative id and a user-style positive
	// one, so the decimal Recipient covers the sign. They are passed in ascending order.
	chatA int64 = -1001000000001
	chatB int64 = 1000000002

	// fakeBaseURL is the base URL every fake-transport case hands to telegram.New. The fake never
	// dials it.
	fakeBaseURL = "https://telegram.example.com"

	// partLimit is the most UTF-16 code units one message may hold (research R9: 4000, safely
	// below Telegram's 4096).
	partLimit = 4000

	// minPartGap is the least time between two parts sent to one chat (research R9).
	minPartGap = 1100 * time.Millisecond

	okBody = `{"ok":true,"result":{"message_id":1}}`
)

// reply is one scripted outcome of fakeTelegram: a network error, or a status and body.
type reply struct {
	err    error
	body   string
	status int
}

func okReply() reply {
	return reply{status: http.StatusOK, body: okBody}
}

// tooManyReply is Telegram's flood-control answer: the wait comes in parameters.retry_after, not
// in a Retry-After header, so the notifier must read the body.
func tooManyReply(retryAfter int) reply {
	return reply{
		status: http.StatusTooManyRequests,
		body: fmt.Sprintf(
			`{"ok":false,"error_code":429,"description":"Too Many Requests: retry after %d",`+
				`"parameters":{"retry_after":%d}}`,
			retryAfter, retryAfter,
		),
	}
}

func errorReply(status int, description string) reply {
	return reply{
		status: status,
		body:   fmt.Sprintf(`{"ok":false,"error_code":%d,"description":%q}`, status, description),
	}
}

func networkReply() reply {
	return reply{err: errors.New("read: connection reset by peer")}
}

// sentMessage is one sendMessage call fakeTelegram received, stamped with the bubble's fake time.
type sentMessage struct {
	at     time.Time
	url    string
	text   string
	chatID int64
}

// fakeTelegram is an in-process, synctest-safe stand-in for the Bot API (.claude/CLAUDE.md: no
// httptest.Server and no sockets inside a synctest bubble). Replies are scripted per chat id, in
// order, so the cases hold whether the notifier walks chats one after another or concurrently;
// a chat whose script has run out gets a plain success.
type fakeTelegram struct {
	script map[int64][]reply
	calls  map[int64]int
	sent   []sentMessage
	mu     sync.Mutex
}

func newFakeTelegram(script map[int64][]reply) *fakeTelegram {
	return &fakeTelegram{script: script, calls: make(map[int64]int)}
}

func (f *fakeTelegram) RoundTrip(req *http.Request) (*http.Response, error) {
	raw, err := io.ReadAll(req.Body)
	_ = req.Body.Close()

	if err != nil {
		return nil, fmt.Errorf("fake telegram: read body: %w", err)
	}

	var payload struct {
		Text   string `json:"text"`
		ChatID int64  `json:"chat_id"`
	}

	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("fake telegram: decode body: %w", err)
	}

	f.mu.Lock()
	f.sent = append(f.sent, sentMessage{
		at:     time.Now(),
		url:    req.URL.String(),
		text:   payload.Text,
		chatID: payload.ChatID,
	})

	idx := f.calls[payload.ChatID]
	f.calls[payload.ChatID]++

	next := okReply()
	if script := f.script[payload.ChatID]; idx < len(script) {
		next = script[idx]
	}
	f.mu.Unlock()

	if next.err != nil {
		return nil, next.err
	}

	return &http.Response{
		Status:     strconv.Itoa(next.status),
		StatusCode: next.status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(next.body)),
		Request:    req,
	}, nil
}

func (f *fakeTelegram) snapshot() []sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]sentMessage(nil), f.sent...)
}

// sendOutcome is everything one Send inside a bubble produced, as plain values, so a case can
// assert on it with suite methods after synctest.Test has returned.
type sendOutcome struct {
	start   time.Time
	results []notify.Result
	sent    []sentMessage
	elapsed time.Duration
}

// forChat returns the messages sent to chatID, in the order they were sent.
func (o sendOutcome) forChat(chatID int64) []sentMessage {
	var out []sentMessage

	for _, m := range o.sent {
		if m.chatID == chatID {
			out = append(out, m)
		}
	}

	return out
}

// units counts s the way Telegram measures a message: in UTF-16 code units, so an emoji outside
// the Basic Multilingual Plane counts 2.
func units(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}

	return n
}

// padTo pads s with ASCII dots to exactly width UTF-16 code units. It panics when s is already
// wider, which is a fixture arithmetic bug, not a behavior under test.
func padTo(s string, width int) string {
	return s + strings.Repeat(".", width-units(s))
}

// bodyLine builds the i-th synthetic digest line: a fixed-width date and amount prefix (27
// units), then emoji (1 rune but 2 UTF-16 units each), padded to exactly width units. With 50
// emoji and width 200 the line is 150 runes but 200 units, so a splitter counting runes packs
// far more lines per message than one counting UTF-16 units.
func bodyLine(i, emoji, width int) string {
	prefix := fmt.Sprintf("- 2026-09-%02d  -%03d.50 EUR  ", i%28+1, i)

	return padTo(prefix+strings.Repeat("💳", emoji), width)
}

func makeDigest(header string, body []string) digest.Digest {
	return digest.Digest{Subject: header, Lines: append([]string{header}, body...)}
}

func joinLines(lines []string) string {
	return strings.Join(lines, "\n")
}

// TelegramSuite covers the Telegram notifier (T047; FR-023, FR-027, FR-028; research R9;
// contracts/digest.md "Telegram splitting").
//
// Pinned message format: a part's text is its lines joined by "\n" with no trailing newline.
// A digest that fits in partLimit UTF-16 units goes out as one message, unchanged. A longer one
// is split greedily at line boundaries: each part takes as many whole lines as fit. The first
// part starts with the header line unchanged; part k of n (k >= 2) starts with the header line
// plus " (k/n)", and that suffix counts toward the part's limit.
//
// Request-shape and 4xx secrecy cases run against httptest. Every case that waits (pacing
// between parts, retries) or sends several parts runs inside a testing/synctest bubble against
// fakeTelegram injected through the *http.Client, so no case sleeps for real. synctest.Test takes
// its own *testing.T, so bubbles only collect plain values and the suite asserts afterwards.
type TelegramSuite struct {
	suite.Suite
}

func (s *TelegramSuite) TestNameIsTelegram() {
	var n notify.Notifier = telegram.New(testToken, []int64{chatA}, http.DefaultClient, fakeBaseURL)

	s.Equal("telegram", n.Name())
}

func (s *TelegramSuite) TestRequestShape() {
	type captured struct {
		method      string
		path        string
		contentType string
		body        []byte
	}

	var (
		mu   sync.Mutex
		reqs []captured
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		mu.Lock()
		reqs = append(reqs, captured{
			method:      r.Method,
			path:        r.URL.Path,
			contentType: r.Header.Get("Content-Type"),
			body:        body,
		})
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, okBody)
	}))
	defer server.Close()

	d := makeDigest(
		"firefly-jar: 1 missing, 0 unchecked accounts (window 2026-08-24 – 2026-09-22)",
		[]string{"", "Missing in Firefly III", "- 2026-09-21  -4.50 EUR  COFFEE SHOP  ⏳ pending"},
	)

	results := telegram.New(testToken, []int64{chatA}, server.Client(), server.URL).Send(s.T().Context(), d)

	s.Require().Len(results, 1)
	s.Equal(strconv.FormatInt(chatA, 10), results[0].Recipient)
	s.Require().NoError(results[0].Err)

	mu.Lock()
	defer mu.Unlock()

	s.Require().Len(reqs, 1, "a digest that fits one message is sent as exactly one request")
	s.Equal(http.MethodPost, reqs[0].method)
	s.Equal("/bot"+testToken+"/sendMessage", reqs[0].path)
	s.True(strings.HasPrefix(reqs[0].contentType, "application/json"), "content type %q", reqs[0].contentType)

	var body map[string]json.RawMessage
	s.Require().NoError(json.Unmarshal(reqs[0].body, &body))

	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}

	s.ElementsMatch([]string{"chat_id", "text", "link_preview_options"}, keys, "the body carries exactly these keys")
	s.NotContains(body, "parse_mode", "plain text only: an arbitrary bank description must never parse as markup")

	// Decoding into an int64 without the ",string" option rejects a quoted "-1001000000001", so
	// this fails unless chat_id is a bare JSON number.
	var chatID int64
	s.Require().NoError(json.Unmarshal(body["chat_id"], &chatID), "chat_id is a JSON number, not a string")
	s.Equal(chatA, chatID)

	var text string
	s.Require().NoError(json.Unmarshal(body["text"], &text))
	s.Equal(joinLines(d.Lines), text, "the text is the lines joined by newlines, with no trailing newline")

	s.JSONEq(`{"is_disabled":true}`, string(body["link_preview_options"]))
}

func (s *TelegramSuite) TestEmptyBaseURLMeansTheBotAPI() {
	fake := newFakeTelegram(nil)
	n := telegram.New(testToken, []int64{chatA}, &http.Client{Transport: fake}, "")

	results := n.Send(s.T().Context(), makeDigest("firefly-jar: 1 missing", []string{"- line"}))

	s.Require().Len(results, 1)
	s.Require().NoError(results[0].Err)

	sent := fake.snapshot()
	s.Require().Len(sent, 1)
	s.Equal("https://api.telegram.org/bot"+testToken+"/sendMessage", sent[0].url)
}

// TestDigestAtTheLimitIsOneMessage builds a digest of exactly partLimit UTF-16 units, emoji
// included, and expects it unsplit.
func (s *TelegramSuite) TestDigestAtTheLimitIsOneMessage() {
	d := s.limitDigest(0)
	s.Require().Equal(partLimit, units(joinLines(d.Lines)), "fixture arithmetic")

	out := s.sendInBubble(nil, []int64{chatA}, d)

	s.Require().Len(out.results, 1)
	s.Require().NoError(out.results[0].Err)
	s.Require().Len(out.sent, 1, "exactly partLimit units still fits one message")
	s.Equal(joinLines(d.Lines), out.sent[0].text)
}

// TestOneUnitOverTheLimitSplitsInTwo adds one unit to the at-limit digest. Its rune count stays
// far below partLimit, so a splitter that counts runes instead of UTF-16 units sends one message.
func (s *TelegramSuite) TestOneUnitOverTheLimitSplitsInTwo() {
	d := s.limitDigest(1)
	s.Require().Equal(partLimit+1, units(joinLines(d.Lines)), "fixture arithmetic")
	s.Require().Less(len([]rune(joinLines(d.Lines))), partLimit, "fixture: rune count alone would not split")

	out := s.sendInBubble(nil, []int64{chatA}, d)

	s.Require().Len(out.results, 1)
	s.Require().NoError(out.results[0].Err)
	s.Require().Len(out.sent, 2)

	header := d.Lines[0]
	last := len(d.Lines) - 1

	s.Equal(joinLines(d.Lines[:last]), out.sent[0].text, "the first part keeps the header unchanged")
	s.Equal(joinLines([]string{header + " (2/2)", d.Lines[last]}), out.sent[1].text)
}

// TestSplitCountsUTF16UnitsAndTheContinuationSuffix pins the whole split of a three-part digest.
// The header is 180 units and every body line 200 units (150 runes), so with the "\n" separators:
//   - part 1: 180 + 19*201 = 3999 fits, a 20th line would not;
//   - parts 2 and 3: the " (k/3)" suffix makes the header 186, and 186 + 18*201 = 3804 fits while
//     a 19th line (4005) would not, so the suffix must count toward the limit;
//   - 45 body lines split 19 + 18 + 8.
//
// Counting runes instead would fit 25 lines per part and send two parts.
func (s *TelegramSuite) TestSplitCountsUTF16UnitsAndTheContinuationSuffix() {
	header, body := s.threePartFixture()
	d := makeDigest(header, body)

	out := s.sendInBubble(nil, []int64{chatA}, d)

	s.Require().Len(out.results, 1)
	s.Require().NoError(out.results[0].Err)
	s.Require().Len(out.sent, 3)

	s.Equal(joinLines(append([]string{header}, body[0:19]...)), out.sent[0].text)
	s.Equal(joinLines(append([]string{header + " (2/3)"}, body[19:37]...)), out.sent[1].text)
	s.Equal(joinLines(append([]string{header + " (3/3)"}, body[37:45]...)), out.sent[2].text)

	var rejoined []string

	for i, m := range out.sent {
		s.LessOrEqual(units(m.text), partLimit, "part %d exceeds the limit", i+1)

		lines := strings.Split(m.text, "\n")
		if i == 0 {
			s.Equal(header, lines[0], "the first part's header is unchanged")

			rejoined = append(rejoined, lines...)

			continue
		}

		s.Equal(fmt.Sprintf("%s (%d/%d)", header, i+1, len(out.sent)), lines[0])

		rejoined = append(rejoined, lines[1:]...)
	}

	s.Equal(d.Lines, rejoined, "dropping the repeated headers gives back exactly the original lines")
}

func (s *TelegramSuite) TestPartsToOneChatAreSequentialAndPaced() {
	header, body := s.threePartFixture()

	out := s.sendInBubble(nil, []int64{chatA}, makeDigest(header, body))

	s.Require().Len(out.sent, 3)
	s.Equal(out.start, out.sent[0].at, "the first part goes out at once")
	s.True(strings.HasPrefix(out.sent[0].text, header+"\n"), "part 1 is sent first")
	s.True(strings.HasPrefix(out.sent[1].text, header+" (2/3)\n"), "part 2 is sent second")
	s.True(strings.HasPrefix(out.sent[2].text, header+" (3/3)\n"), "part 3 is sent third")

	for i := 1; i < len(out.sent); i++ {
		s.GreaterOrEqual(out.sent[i].at.Sub(out.sent[i-1].at), minPartGap, "gap before part %d", i+1)
	}
}

func (s *TelegramSuite) TestTooManyRequestsWaitsRetryAfterThenRetries() {
	cases := []struct {
		name       string
		retryAfter int
	}{
		{name: "3s", retryAfter: 3},
		{name: "at the 60s cap", retryAfter: 60},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			script := map[int64][]reply{chatA: {tooManyReply(tc.retryAfter), okReply()}}

			out := s.sendInBubble(script, []int64{chatA}, s.oneLineDigest())

			s.Require().Len(out.results, 1)
			s.Require().NoError(out.results[0].Err)
			s.Require().Len(out.sent, 2)
			s.Equal(
				out.start.Add(time.Duration(tc.retryAfter)*time.Second), out.sent[1].at,
				"the retry waits exactly parameters.retry_after",
			)
		})
	}
}

func (s *TelegramSuite) TestRetryAfterOverTheCapFailsWithoutWaiting() {
	script := map[int64][]reply{chatA: {tooManyReply(61), okReply()}}

	out := s.sendInBubble(script, []int64{chatA}, s.oneLineDigest())

	s.Require().Len(out.results, 1)
	s.Require().Error(out.results[0].Err)
	s.Len(out.sent, 1, "a wait over 60s is never attempted")
	s.Equal(time.Duration(0), out.elapsed, "a wait over 60s is not waited out, even partially")
}

func (s *TelegramSuite) TestServerAndNetworkErrorsAreRetriedAfterOneSecond() {
	cases := []struct {
		first reply
		name  string
	}{
		{name: "500", first: errorReply(http.StatusInternalServerError, "Internal Server Error")},
		{name: "502", first: errorReply(http.StatusBadGateway, "Bad Gateway")},
		{name: "503", first: errorReply(http.StatusServiceUnavailable, "Service Unavailable")},
		{name: "network error", first: networkReply()},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			script := map[int64][]reply{chatA: {tc.first, okReply()}}

			out := s.sendInBubble(script, []int64{chatA}, s.oneLineDigest())

			s.Require().Len(out.results, 1)
			s.Require().NoError(out.results[0].Err)
			s.Require().Len(out.sent, 2)
			s.Equal(out.start.Add(time.Second), out.sent[1].at, "the first retry waits exactly 1s")
		})
	}
}

func (s *TelegramSuite) TestRetriesBackOffOneTwoFourSecondsThenFail() {
	script := map[int64][]reply{chatA: {
		errorReply(http.StatusBadGateway, "Bad Gateway"),
		errorReply(http.StatusBadGateway, "Bad Gateway"),
		errorReply(http.StatusBadGateway, "Bad Gateway"),
		errorReply(http.StatusBadGateway, "Bad Gateway"),
		okReply(), // Unreachable: a fifth attempt is one retry too many.
	}}

	out := s.sendInBubble(script, []int64{chatA}, s.oneLineDigest())

	s.Require().Len(out.results, 1)
	s.Require().Error(out.results[0].Err)
	s.Require().Len(out.sent, 4, "at most 3 retries per part")
	s.Equal(out.start, out.sent[0].at)
	s.Equal(out.start.Add(1*time.Second), out.sent[1].at)
	s.Equal(out.start.Add(3*time.Second), out.sent[2].at)
	s.Equal(out.start.Add(7*time.Second), out.sent[3].at)
}

func (s *TelegramSuite) TestTooManyRequestsCountsTowardTheRetryCap() {
	script := map[int64][]reply{chatA: {
		tooManyReply(1), tooManyReply(1), tooManyReply(1), tooManyReply(1),
		okReply(), // Unreachable: a fifth attempt is one retry too many.
	}}

	out := s.sendInBubble(script, []int64{chatA}, s.oneLineDigest())

	s.Require().Len(out.results, 1)
	s.Require().Error(out.results[0].Err)
	s.Len(out.sent, 4, "a 429 retry counts toward the 3-retry cap")
}

// TestClientErrorsFailTheRecipientAtOnce sends a two-part digest: the first part's 4xx fails the
// recipient, so neither a retry nor the second part is ever sent.
func (s *TelegramSuite) TestClientErrorsFailTheRecipientAtOnce() {
	cases := []struct {
		name        string
		description string
		status      int
	}{
		{name: "400", status: http.StatusBadRequest, description: "Bad Request: chat not found"},
		{name: "401", status: http.StatusUnauthorized, description: "Unauthorized"},
		{name: "403", status: http.StatusForbidden, description: "Forbidden: bot was blocked by the user"},
		{name: "404", status: http.StatusNotFound, description: "Not Found"},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			script := map[int64][]reply{chatA: {errorReply(tc.status, tc.description), okReply()}}

			out := s.sendInBubble(script, []int64{chatA}, s.limitDigest(1))

			s.Require().Len(out.results, 1)
			s.Equal(strconv.FormatInt(chatA, 10), out.results[0].Recipient)
			s.Require().Error(out.results[0].Err)
			s.Contains(out.results[0].Err.Error(), strconv.Itoa(tc.status), "the reason names the status")
			s.Len(out.sent, 1, "no retry and no further part")
			s.Equal(time.Duration(0), out.elapsed, "a client error fails without any wait")
		})
	}
}

func (s *TelegramSuite) TestEachChatIsAttemptedIndependently() {
	exhausted := []reply{
		errorReply(http.StatusBadGateway, "Bad Gateway"),
		errorReply(http.StatusBadGateway, "Bad Gateway"),
		errorReply(http.StatusBadGateway, "Bad Gateway"),
		errorReply(http.StatusBadGateway, "Bad Gateway"),
	}

	cases := []struct {
		script  map[int64][]reply
		name    string
		failing int64
	}{
		{
			name:    "first chat forbidden",
			script:  map[int64][]reply{chatA: {errorReply(http.StatusForbidden, "Forbidden")}},
			failing: chatA,
		},
		{
			name:    "second chat forbidden",
			script:  map[int64][]reply{chatB: {errorReply(http.StatusForbidden, "Forbidden")}},
			failing: chatB,
		},
		{name: "first chat retries exhausted", script: map[int64][]reply{chatA: exhausted}, failing: chatA},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			out := s.sendInBubble(tc.script, []int64{chatA, chatB}, s.limitDigest(1))

			s.Require().Len(out.results, 2, "one result per chat id")
			s.Equal(strconv.FormatInt(chatA, 10), out.results[0].Recipient, "results follow the chat id order")
			s.Equal(strconv.FormatInt(chatB, 10), out.results[1].Recipient, "results follow the chat id order")

			for i, chatID := range []int64{chatA, chatB} {
				if chatID == tc.failing {
					s.Require().Error(out.results[i].Err)
					continue
				}

				s.Require().NoError(out.results[i].Err)
				s.Len(out.forChat(chatID), 2, "the healthy chat still gets every part")
			}
		})
	}
}

// TestTooManyRequestsWithoutRetryAfterWaitsTheBackoff covers a 429 whose body carries no
// parameters.retry_after: with nothing to honor, the retry waits the 5xx backoff (1s first).
func (s *TelegramSuite) TestTooManyRequestsWithoutRetryAfterWaitsTheBackoff() {
	script := map[int64][]reply{chatA: {errorReply(http.StatusTooManyRequests, "Too Many Requests"), okReply()}}

	out := s.sendInBubble(script, []int64{chatA}, s.oneLineDigest())

	s.Require().Len(out.results, 1)
	s.Require().NoError(out.results[0].Err)
	s.Require().Len(out.sent, 2)
	s.Equal(out.start.Add(time.Second), out.sent[1].at, "a 429 without retry_after waits the 1s backoff")
}

// TestOkFalseOnASuccessStatusFailsAtOnce covers a 200 whose body says {"ok":false}: the Bot API
// did not deliver, so it is a failure, and like a 400 it is neither retried nor followed by the
// next part.
func (s *TelegramSuite) TestOkFalseOnASuccessStatusFailsAtOnce() {
	okFalse := reply{
		status: http.StatusOK,
		body:   `{"ok":false,"error_code":400,"description":"Bad Request: message text is empty"}`,
	}
	script := map[int64][]reply{chatA: {okFalse, okReply()}}

	out := s.sendInBubble(script, []int64{chatA}, s.limitDigest(1))

	s.Require().Len(out.results, 1)
	s.Require().Error(out.results[0].Err, "ok:false is a failure even on HTTP 200")
	s.Len(out.sent, 1, "no retry and no further part")
	s.Equal(time.Duration(0), out.elapsed, "an ok:false reply fails without any wait")
}

// TestCancelDuringAWaitFailsPromptly cancels the caller's context in the middle of each kind of
// wait on part 1 of a two-part digest: Send returns at the cancel, not when the wait would have
// ended, the recipient fails with context.Canceled, and nothing more is sent.
func (s *TelegramSuite) TestCancelDuringAWaitFailsPromptly() {
	cases := []struct {
		name   string
		script []reply
	}{
		{name: "retry_after wait", script: []reply{tooManyReply(30), okReply(), okReply()}},
		{
			name:   "server error backoff",
			script: []reply{errorReply(http.StatusBadGateway, "Bad Gateway"), okReply(), okReply()},
		},
		{name: "pacing gap between parts", script: []reply{okReply(), okReply()}},
	}

	const cancelAt = 500 * time.Millisecond

	for _, tc := range cases {
		s.Run(tc.name, func() {
			script := map[int64][]reply{chatA: tc.script}

			out := s.sendInBubbleCancelAfter(script, []int64{chatA}, s.limitDigest(1), cancelAt)

			s.Require().Len(out.results, 1)
			s.Require().ErrorIs(out.results[0].Err, context.Canceled)
			s.Len(out.sent, 1, "no attempt after the cancel")
			s.Equal(cancelAt, out.elapsed, "the wait ends at the cancel")
		})
	}
}

// TestResultsFollowTheConfiguredChatOrder passes the chat ids in descending order, so a notifier
// that sorted them would be told apart from one that keeps the configured order.
func (s *TelegramSuite) TestResultsFollowTheConfiguredChatOrder() {
	out := s.sendInBubble(nil, []int64{chatB, chatA}, s.oneLineDigest())

	s.Require().Len(out.results, 2)
	s.Equal(strconv.FormatInt(chatB, 10), out.results[0].Recipient)
	s.Equal(strconv.FormatInt(chatA, 10), out.results[1].Recipient)
	s.Require().NoError(out.results[0].Err)
	s.Require().NoError(out.results[1].Err)
}

func (s *TelegramSuite) TestClientErrorsNeverContainTheBotToken() {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden} {
		s.Run(strconv.Itoa(status), func() {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)

				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = fmt.Fprintf(w, `{"ok":false,"error_code":%d,"description":"Unauthorized"}`, status)
			}))
			defer server.Close()

			n := telegram.New(testToken, []int64{chatA}, server.Client(), server.URL)
			results := n.Send(s.T().Context(), s.oneLineDigest())

			s.Require().Len(results, 1)
			s.Require().Error(results[0].Err)
			s.assertNoToken(results[0].Err)
		})
	}
}

// TestRetriedFailuresNeverContainTheBotToken covers the failures whose error would naturally
// carry the request URL, and so the token: http.Client wraps a transport error in a *url.Error
// that prints the full URL.
func (s *TelegramSuite) TestRetriedFailuresNeverContainTheBotToken() {
	cases := []struct {
		name  string
		reply reply
	}{
		{name: "network error", reply: networkReply()},
		{name: "server error", reply: errorReply(http.StatusBadGateway, "Bad Gateway")},
		{name: "retry_after over the cap", reply: tooManyReply(3600)},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			script := map[int64][]reply{chatA: {tc.reply, tc.reply, tc.reply, tc.reply}}

			out := s.sendInBubble(script, []int64{chatA}, s.oneLineDigest())

			s.Require().Len(out.results, 1)
			s.Require().Error(out.results[0].Err)
			s.assertNoToken(out.results[0].Err)
		})
	}
}

// sendInBubble builds a notifier on fakeTelegram inside a synctest bubble, sends d once and
// returns what happened. The bubble's fake clock makes every wait instant and exact.
func (s *TelegramSuite) sendInBubble(script map[int64][]reply, chatIDs []int64, d digest.Digest) sendOutcome {
	return s.sendInBubbleCancelAfter(script, chatIDs, d, 0)
}

// sendInBubbleCancelAfter is sendInBubble with the context handed to Send cancelled cancelAfter
// into the bubble's fake time; zero never cancels it.
func (s *TelegramSuite) sendInBubbleCancelAfter(
	script map[int64][]reply, chatIDs []int64, d digest.Digest, cancelAfter time.Duration,
) sendOutcome {
	var out sendOutcome

	synctest.Test(s.T(), func(t *testing.T) {
		fake := newFakeTelegram(script)
		n := telegram.New(testToken, chatIDs, &http.Client{Transport: fake}, fakeBaseURL)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		if cancelAfter > 0 {
			timer := time.AfterFunc(cancelAfter, cancel)
			defer timer.Stop()
		}

		out.start = time.Now()
		out.results = n.Send(ctx, d)
		out.elapsed = time.Since(out.start)
		out.sent = fake.snapshot()
	})

	return out
}

func (s *TelegramSuite) assertNoToken(err error) {
	s.NotContains(err.Error(), testToken, "an error must never carry the bot token")
	s.NotContains(err.Error(), testTokenSecret, "an error must never carry the token's secret half")
}

func (s *TelegramSuite) oneLineDigest() digest.Digest {
	return makeDigest("firefly-jar: 1 missing", []string{"- 2026-09-21  -4.50 EUR  COFFEE SHOP"})
}

// limitDigest builds a realistic header plus 18 emoji-heavy 200-unit lines and a last line padded
// so the whole text is partLimit+extra UTF-16 units.
func (s *TelegramSuite) limitDigest(extra int) digest.Digest {
	header := "firefly-jar: 19 missing, 0 unchecked accounts (window 2026-08-24 – 2026-09-22)"

	body := make([]string, 0, 19)
	for i := range 18 {
		body = append(body, bodyLine(i, 50, 200))
	}

	sofar := units(joinLines(append([]string{header}, body...)))
	body = append(body, bodyLine(18, 50, partLimit+extra-sofar-1))

	return makeDigest(header, body)
}

// threePartFixture returns a 180-unit header (one emoji included) and 45 body lines of 200 units
// each; TestSplitCountsUTF16UnitsAndTheContinuationSuffix spells out the arithmetic.
func (s *TelegramSuite) threePartFixture() (string, []string) {
	header := padTo("firefly-jar: 45 missing, 0 unchecked accounts (window 2026-08-24 – 2026-09-22) 🧾 ", 180)

	body := make([]string, 0, 45)
	for i := range 45 {
		body = append(body, bodyLine(i, 50, 200))
	}

	return header, body
}

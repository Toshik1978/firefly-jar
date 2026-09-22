package redact_test

import (
	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/redact"
)

// RedactorSuite covers redact.MaskIBAN, redact.MaskHash and the Redactor built by redact.New:
// masking IBAN- and hash-shaped identifiers, and scrubbing configured secrets and Telegram bot
// tokens out of free text (FR-035, FR-036).
type RedactorSuite struct {
	suite.Suite
}

func (s *RedactorSuite) TestMaskIBANShowsFirstAndLastFour() {
	got := redact.MaskIBAN("LT121000011101001000")

	s.Equal("LT12…1000", got)
}

func (s *RedactorSuite) TestMaskIBANAnonymizedFixtureStyle() {
	// Anonymized-fixture-style IBAN, per .claude/CLAUDE.md's masked-IBAN examples.
	got := redact.MaskIBAN("LT000000000000000001")

	s.Equal("LT00…0001", got)
}

func (s *RedactorSuite) TestMaskIBANShortStringsComeBackAsAsterisks() {
	cases := map[string]string{
		"empty string":       "",
		"one character":      "L",
		"seven characters":   "LT12100",
		"exactly eight":      "LT121000",
		"exactly eight zero": "00000000",
	}

	for name, in := range cases {
		s.Run(name, func() {
			s.Equal("****", redact.MaskIBAN(in))
		})
	}
}

func (s *RedactorSuite) TestMaskHashShowsFirstAndLastFour() {
	got := redact.MaskHash("0123456789abcdef0123456789abcdef")

	s.Equal("hash:0123…cdef", got)
}

func (s *RedactorSuite) TestMaskHashShortStringsComeBackAsAsterisks() {
	cases := map[string]string{
		"empty string":       "",
		"one character":      "h",
		"seven characters":   "0123456",
		"exactly eight":      "01234567",
		"exactly eight zero": "00000000",
	}

	for name, in := range cases {
		s.Run(name, func() {
			s.Equal("hash:****", redact.MaskHash(in))
		})
	}
}

func (s *RedactorSuite) TestNewScrubReplacesEveryConfiguredSecretWithRedacted() {
	r := redact.New("s3cr3t-token-aaa", "s3cr3t-token-bbb")

	got := r.Scrub("token=s3cr3t-token-aaa other=s3cr3t-token-bbb unrelated=plain-value")

	s.Equal("token=[REDACTED] other=[REDACTED] unrelated=plain-value", got)
}

func (s *RedactorSuite) TestNewScrubEmptySecretDoesNotBlankOutText() {
	r := redact.New("", "", "s3cr3t-token-aaa")

	got := r.Scrub("this text has no secret in it, only s3cr3t-token-aaa")

	s.Equal("this text has no secret in it, only [REDACTED]", got)
}

func (s *RedactorSuite) TestScrubMasksIBANShapedSubstringsInFreeText() {
	r := redact.New()

	got := r.Scrub("bank fetch failed for account LT121000011101001000: timeout")

	s.Equal("bank fetch failed for account LT12…1000: timeout", got)
	s.NotContains(got, "LT121000011101001000")
}

func (s *RedactorSuite) TestScrubMasksMultipleIBANShapedSubstrings() {
	r := redact.New()

	got := r.Scrub("moved from LT121000011101001000 to DE00500105175407324931")

	s.Equal("moved from LT12…1000 to DE00…4931", got)
}

func (s *RedactorSuite) TestScrubLeavesIBANShapedSubstringEmbeddedInLongerTokenAlone() {
	r := redact.New()

	got := r.Scrub("XLT121000011101001000XYZ")

	s.Equal("XLT121000011101001000XYZ", got)
}

func (s *RedactorSuite) TestScrubMasksIBANFollowedByPunctuationExactly() {
	r := redact.New()
	cases := map[string]struct {
		in   string
		want string
	}{
		"trailing comma":    {in: "LT121000011101001000,", want: "LT12…1000,"},
		"wrapped in parens": {in: "(LT121000011101001000)", want: "(LT12…1000)"},
	}

	for name, tc := range cases {
		s.Run(name, func() {
			s.Equal(tc.want, r.Scrub(tc.in))
		})
	}
}

func (s *RedactorSuite) TestScrubMasksLowercaseIBANShapedSubstrings() {
	r := redact.New()

	got := r.Scrub("acct lt121000011101001000 end")

	s.Equal("acct lt12…1000 end", got)
}

func (s *RedactorSuite) TestScrubMasksTelegramBotTokenEvenWhenNotAConfiguredSecret() {
	r := redact.New()
	token := "123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11"

	got := r.Scrub("https://api.telegram.org/bot" + token + "/sendMessage")

	s.Equal("https://api.telegram.org/bot[REDACTED]/sendMessage", got)
	s.NotContains(got, token, "the bot token must never survive Scrub, configured secret or not")
}

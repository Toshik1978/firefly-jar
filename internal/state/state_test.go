package state_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/state"
)

// TestState is the single entry point for package state's test suites.
func TestState(t *testing.T) {
	suite.Run(t, new(StateSuite))
}

// copyFixtureFile copies testdata/state/name into a fresh t.TempDir() under the same base name, so
// tests can chmod or overwrite their own copy without ever mutating the shared fixture.
func copyFixtureFile(t *testing.T, name string) string {
	t.Helper()

	src := filepath.Join("..", "..", "testdata", "state", name)

	data, err := os.ReadFile(filepath.Clean(src))
	if err != nil {
		t.Fatalf("read fixture %s: %v", src, err)
	}

	dst := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatalf("write fixture copy %s: %v", dst, err)
	}

	return dst
}

// mustParseTime parses an RFC3339 timestamp, failing the test on error, so fixture-matching
// expectations read as plain literals instead of ignored error returns.
func mustParseTime(t *testing.T, value string) time.Time {
	t.Helper()

	tm, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}

	return tm
}

// StateSuite covers internal/state: Load/Save of the bank session file, atomic writes and Put's
// per-bank replacement (T021, contracts/state.md, FR-019, FR-037, research R11).
type StateSuite struct {
	suite.Suite
}

// requireSessionEqual asserts two Sessions are equal, comparing time fields with time.Time.Equal so
// that equivalent instants in different (but round-trip-preserving) representations still match.
func (s *StateSuite) requireSessionEqual(want, got state.Session) {
	s.Equal(want.Provider, got.Provider)
	s.Equal(want.SessionID, got.SessionID)
	s.True(want.ValidUntil.Equal(got.ValidUntil), "ValidUntil: want %v, got %v", want.ValidUntil, got.ValidUntil)
	s.True(
		want.AuthorizedAt.Equal(got.AuthorizedAt),
		"AuthorizedAt: want %v, got %v", want.AuthorizedAt, got.AuthorizedAt,
	)
	s.Equal(want.Accounts, got.Accounts)
}

// requireStateEqual asserts two States are equal, delegating session comparison to
// requireSessionEqual so time fields compare by instant rather than by representation.
func (s *StateSuite) requireStateEqual(want, got *state.State) {
	s.Require().NotNil(got)
	s.Equal(want.Version, got.Version)
	s.Require().Len(got.Sessions, len(want.Sessions))

	for key, wantSess := range want.Sessions {
		gotSess, ok := got.Sessions[key]
		s.Require().True(ok, "missing session for bank %q", key)
		s.requireSessionEqual(wantSess, gotSess)
	}
}

func (s *StateSuite) TestLoadOfMissingPathReturnsEmptyStateWithoutError() {
	path := filepath.Join(s.T().TempDir(), "does-not-exist.json")

	st, warnings, err := state.Load(path)

	s.Require().NoError(err)
	s.Empty(warnings)
	s.Require().NotNil(st)
	s.Equal(1, st.Version)
	s.Require().NotNil(st.Sessions)
	s.Empty(st.Sessions)
}

func (s *StateSuite) TestLoadValidJSONRoundTrips() {
	path := copyFixtureFile(s.T(), "valid.json")

	st, warnings, err := state.Load(path)

	s.Require().NoError(err)
	s.Empty(warnings)
	s.Require().NotNil(st)
	s.Equal(1, st.Version)
	s.Require().Len(st.Sessions, 2)

	swedbank, ok := st.Sessions["swedbank"]
	s.Require().True(ok)
	s.Equal("enablebanking", swedbank.Provider)
	s.Equal("00000000-0000-0000-0000-000000000001", swedbank.SessionID)
	s.True(swedbank.ValidUntil.Equal(mustParseTime(s.T(), "2027-03-21T10:15:00Z")))
	s.True(swedbank.AuthorizedAt.Equal(mustParseTime(s.T(), "2026-09-22T10:15:00Z")))
	s.Require().Len(swedbank.Accounts, 2)
	s.Equal(state.Account{
		UID:      "00000000-0000-0000-0000-000000000001",
		Hash:     "WwpbCiJhY2NvdW50IiwKImFjY291bnRfaWQiLAoiaWJhbiIKXQpd.E8Gz",
		IBAN:     "LT000000000000000001",
		Currency: "EUR",
		Name:     "Main",
	}, swedbank.Accounts[0])
	s.Equal(state.Account{
		UID:      "00000000-0000-0000-0000-000000000002",
		Hash:     "YnJhbmNoL2Q4YzlkZjI4OzAvMTAwMQo=.xYZ123",
		IBAN:     "",
		Currency: "EUR",
		Name:     "Card",
	}, swedbank.Accounts[1])

	revolut, ok := st.Sessions["revolut"]
	s.Require().True(ok)
	s.Equal("enablebanking", revolut.Provider)
	s.Equal("00000000-0000-0000-0000-000000000002", revolut.SessionID)
	s.True(revolut.ValidUntil.Equal(mustParseTime(s.T(), "2027-04-15T14:30:00Z")))
	s.True(revolut.AuthorizedAt.Equal(mustParseTime(s.T(), "2026-09-20T14:30:00Z")))
	s.Require().Len(revolut.Accounts, 1)
	s.Equal(state.Account{
		UID:      "00000000-0000-0000-0000-000000000003",
		Hash:     "QWxwaXMvMTAwMDI7L21haW4vYWNjdA==.aB9Cd",
		IBAN:     "LT000000000000000002",
		Currency: "USD",
		Name:     "Main",
	}, revolut.Accounts[0])

	// Round-trip: Save the loaded state to a fresh path, then Load it back and require equality.
	roundTripPath := filepath.Join(s.T().TempDir(), "roundtrip.json")
	s.Require().NoError(state.Save(roundTripPath, st))

	reloaded, reloadWarnings, err := state.Load(roundTripPath)
	s.Require().NoError(err)
	s.Empty(reloadWarnings)
	s.requireStateEqual(st, reloaded)
}

func (s *StateSuite) TestLoadStatFailureOtherThanNotExistIsError() {
	s.skipIfRoot()

	dir := s.T().TempDir()
	sub := filepath.Join(dir, "sub")
	s.Require().NoError(os.Mkdir(sub, 0o700))
	path := filepath.Join(sub, "state.json")
	s.Require().NoError(os.WriteFile(path, []byte("{}"), 0o600))

	// A directory without search (execute) permission makes os.Stat on a file inside it fail with
	// permission denied instead of ErrNotExist, exercising Load's generic stat-error branch.
	s.Require().NoError(os.Chmod(sub, 0o600))
	s.T().Cleanup(func() { _ = os.Chmod(sub, 0o700) })

	st, warnings, err := state.Load(path)

	s.Require().Error(err)
	s.Nil(st)
	s.Empty(warnings)
	s.ErrorContains(err, "stat state file")
}

func (s *StateSuite) TestLoadOfUnreadableFileIsError() {
	s.skipIfRoot()

	path := copyFixtureFile(s.T(), "valid.json")
	s.Require().NoError(os.Chmod(path, 0o000))
	s.T().Cleanup(func() { _ = os.Chmod(path, 0o600) })

	st, warnings, err := state.Load(path)

	s.Require().Error(err)
	s.Nil(st)
	s.Empty(warnings)
	s.ErrorContains(err, "read state file")
}

func (s *StateSuite) TestLoadMalformedJSONIsError() {
	path := copyFixtureFile(s.T(), "malformed.json")

	st, warnings, err := state.Load(path)

	s.Require().Error(err)
	s.Nil(st)
	s.Empty(warnings)
	s.ErrorContains(err, "decode state file")
}

func (s *StateSuite) TestLoadWithoutSessionsKeyReturnsEmptyMap() {
	path := copyFixtureFile(s.T(), "no_sessions.json")

	st, warnings, err := state.Load(path)

	s.Require().NoError(err)
	s.Empty(warnings)
	s.Require().NotNil(st)
	s.Equal(1, st.Version)
	s.Require().NotNil(st.Sessions)
	s.Empty(st.Sessions)
}

func (s *StateSuite) TestLoadUnknownVersionIsError() {
	path := copyFixtureFile(s.T(), "bad_version.json")

	st, warnings, err := state.Load(path)

	s.Require().Error(err)
	s.Nil(st)
	s.Empty(warnings)
	s.ErrorContains(err, "version")
}

func (s *StateSuite) TestLoadWithGroupOrOtherPermissionBitsReturnsWarning() {
	s.skipIfRoot()

	path := copyFixtureFile(s.T(), "valid.json")
	s.Require().NoError(os.Chmod(path, 0o644))

	st, warnings, err := state.Load(path)

	s.Require().NoError(err)
	s.Require().NotNil(st)
	s.NotEmpty(warnings)
	s.True(containsSubstring(warnings, path), "warnings %v should name path %s", warnings, path)
}

// skipIfRoot skips permission-denial cases, which root bypasses.
func (s *StateSuite) skipIfRoot() {
	if os.Geteuid() == 0 {
		s.T().Skip("root bypasses file permission checks")
	}
}

// containsSubstring reports whether any element of list contains sub.
func containsSubstring(list []string, sub string) bool {
	for _, item := range list {
		if strings.Contains(item, sub) {
			return true
		}
	}

	return false
}

func (s *StateSuite) TestSaveWritesMode0600() {
	dir := s.T().TempDir()
	path := filepath.Join(dir, "state.json")
	st := &state.State{Version: 1, Sessions: map[string]state.Session{}}

	err := state.Save(path, st)

	s.Require().NoError(err)
	info, statErr := os.Stat(path)
	s.Require().NoError(statErr)
	s.Equal(os.FileMode(0o600), info.Mode().Perm())
}

func (s *StateSuite) TestSaveRenameFailureIsError() {
	dir := s.T().TempDir()
	path := filepath.Join(dir, "state.json")

	// A directory sitting at the destination path makes os.Rename fail after the temp file has
	// already been written, exercising Save's rename-error branch distinctly from the
	// create-temp-file failure covered by TestSaveToReadOnlyDirLeavesOldFileIntactAndNoTempFileBehind.
	s.Require().NoError(os.Mkdir(path, 0o700))

	st := &state.State{Version: 1, Sessions: map[string]state.Session{}}

	err := state.Save(path, st)

	s.Require().ErrorContains(err, "rename state file")

	entries, readErr := os.ReadDir(dir)
	s.Require().NoError(readErr)
	for _, entry := range entries {
		s.NotContains(entry.Name(), ".tmp", "leftover temp file: %s", entry.Name())
	}
}

func (s *StateSuite) TestPutOnZeroValueStateInitializesSessions() {
	var st state.State

	st.Put("swedbank", state.Session{Provider: "enablebanking", SessionID: "00000000-0000-0000-0000-000000000001"})

	s.Require().NotNil(st.Sessions)
	s.Require().Len(st.Sessions, 1)
	s.Equal("00000000-0000-0000-0000-000000000001", st.Sessions["swedbank"].SessionID)
}

func (s *StateSuite) TestPutReplacesOnlyThatBank() {
	path := copyFixtureFile(s.T(), "valid.json")
	st, warnings, err := state.Load(path)
	s.Require().NoError(err)
	s.Empty(warnings)
	originalRevolut := st.Sessions["revolut"]

	newSession := state.Session{
		Provider:     "enablebanking",
		SessionID:    "00000000-0000-0000-0000-0000000000ff",
		ValidUntil:   mustParseTime(s.T(), "2028-01-01T00:00:00Z"),
		AuthorizedAt: mustParseTime(s.T(), "2026-09-23T00:00:00Z"),
		Accounts: []state.Account{
			{
				UID:      "00000000-0000-0000-0000-0000000000fe",
				Hash:     "ZmFrZS1oYXNo",
				IBAN:     "LT000000000000000009",
				Currency: "EUR",
				Name:     "Replaced",
			},
		},
	}

	st.Put("swedbank", newSession)
	s.Require().NoError(state.Save(path, st))

	reloaded, reloadWarnings, err := state.Load(path)
	s.Require().NoError(err)
	s.Empty(reloadWarnings)
	s.Require().Len(reloaded.Sessions, 2)
	s.requireSessionEqual(newSession, reloaded.Sessions["swedbank"])
	s.requireSessionEqual(originalRevolut, reloaded.Sessions["revolut"])
}

func (s *StateSuite) TestSaveToReadOnlyDirLeavesOldFileIntactAndNoTempFileBehind() {
	s.skipIfRoot()

	dir := s.T().TempDir()
	path := filepath.Join(dir, "state.json")

	initial := &state.State{
		Version: 1,
		Sessions: map[string]state.Session{
			"swedbank": {
				Provider:     "enablebanking",
				SessionID:    "00000000-0000-0000-0000-000000000001",
				ValidUntil:   mustParseTime(s.T(), "2027-03-21T10:15:00Z"),
				AuthorizedAt: mustParseTime(s.T(), "2026-09-22T10:15:00Z"),
				Accounts:     nil,
			},
		},
	}
	s.Require().NoError(state.Save(path, initial))

	originalBytes, err := os.ReadFile(filepath.Clean(path))
	s.Require().NoError(err)

	s.Require().NoError(os.Chmod(dir, 0o500))
	s.T().Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	modified := &state.State{
		Version: 1,
		Sessions: map[string]state.Session{
			"revolut": {
				Provider:     "enablebanking",
				SessionID:    "00000000-0000-0000-0000-000000000002",
				ValidUntil:   mustParseTime(s.T(), "2027-04-15T14:30:00Z"),
				AuthorizedAt: mustParseTime(s.T(), "2026-09-20T14:30:00Z"),
				Accounts:     nil,
			},
		},
	}

	saveErr := state.Save(path, modified)
	s.Require().Error(saveErr)

	afterBytes, err := os.ReadFile(filepath.Clean(path))
	s.Require().NoError(err)
	s.Equal(originalBytes, afterBytes)

	entries, err := os.ReadDir(dir)
	s.Require().NoError(err)
	for _, entry := range entries {
		s.NotContains(entry.Name(), ".tmp", "leftover temp file: %s", entry.Name())
	}
}

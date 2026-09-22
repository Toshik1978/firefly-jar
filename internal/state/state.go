// Package state persists which banks are authorized and which accounts each session last saw
// (contracts/state.md). It never holds a transaction (FR-037) and is written only by auth; check and
// accounts only read it.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// CurrentVersion is the only state file version this build understands (contracts/state.md, FR-030).
const CurrentVersion = 1

// permissionWarnBits are the group/other permission bits that make Load warn about a state file,
// because the file carries bank session identifiers (research R11).
const permissionWarnBits = 0o077

// tempFilePattern names the temp file Save writes before renaming it into place (research R11).
const tempFilePattern = ".state.*.tmp"

// State is the bank session file: which banks are authorized, and which accounts each session
// covers.
type State struct {
	Version  int                `json:"version"`
	Sessions map[string]Session `json:"sessions"`
}

// Session is one bank's Enable Banking consent: how long it lasts and which accounts it covers.
type Session struct {
	Provider     string    `json:"provider"`
	SessionID    string    `json:"session_id"`
	ValidUntil   time.Time `json:"valid_until"`
	AuthorizedAt time.Time `json:"authorized_at"`
	Accounts     []Account `json:"accounts"`
}

// Account is one bank account a session saw, keyed by the provider's own identifiers rather than by
// account number: uid changes on every re-authorization, but hash does not (research R4).
type Account struct {
	UID      string `json:"uid"`
	Hash     string `json:"hash"`
	IBAN     string `json:"iban"`
	Currency string `json:"currency"`
	Name     string `json:"name"`
}

// Load reads the state file at path. A missing file is not an error: it means no bank has ever been
// authorized, so Load returns an empty State at CurrentVersion. An unknown version is an error
// (contracts/state.md, FR-030). A file readable by group or other produces a warning naming path,
// never its contents, because the file carries session identifiers (research R11).
func Load(path string) (*State, []string, error) {
	clean := filepath.Clean(path)

	info, err := os.Stat(clean)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return &State{Version: CurrentVersion, Sessions: map[string]Session{}}, nil, nil
	case err != nil:
		return nil, nil, fmt.Errorf("stat state file: %w", err)
	}

	var warnings []string
	if info.Mode().Perm()&permissionWarnBits != 0 {
		warnings = append(warnings, fmt.Sprintf("state file %s is readable by group or other", clean))
	}

	data, err := os.ReadFile(clean)
	if err != nil {
		return nil, nil, fmt.Errorf("read state file: %w", err)
	}

	var st State
	if err = json.Unmarshal(data, &st); err != nil {
		return nil, nil, fmt.Errorf("decode state file: %w", err)
	}

	if st.Version != CurrentVersion {
		return nil, nil, fmt.Errorf("state file %s: unknown version %d", clean, st.Version)
	}

	if st.Sessions == nil {
		st.Sessions = map[string]Session{}
	}

	return &st, warnings, nil
}

// Save writes st to path atomically (research R11): a temp file in the same directory is chmod'd
// 0600, written, synced, closed and renamed over path, and the directory itself is then synced so
// the rename survives a crash. On any error the temp file is removed and path is left untouched.
func Save(path string, st *State) error {
	clean := filepath.Clean(path)
	dir := filepath.Dir(clean)

	tmp, err := os.CreateTemp(dir, tempFilePattern)
	if err != nil {
		return fmt.Errorf("create temp state file: %w", err)
	}

	tmpPath := tmp.Name()

	if err = writeState(tmp, st); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)

		return err
	}

	if err = os.Rename(tmpPath, clean); err != nil {
		_ = os.Remove(tmpPath)

		return fmt.Errorf("rename state file: %w", err)
	}

	if err = syncDir(dir); err != nil {
		return fmt.Errorf("sync state directory: %w", err)
	}

	return nil
}

// Put replaces the entry for bankKey, leaving every other bank's session untouched
// (contracts/state.md: "auth <bank> replaces only that bank's entry").
func (s *State) Put(bankKey string, session Session) {
	if s.Sessions == nil {
		s.Sessions = map[string]Session{}
	}

	s.Sessions[bankKey] = session
}

// writeState chmod's f to 0600, encodes st into it and syncs and closes it, in the order research
// R11 requires before the caller renames it into place.
func writeState(f *os.File, st *State) error {
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod temp state file: %w", err)
	}

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")

	if err := enc.Encode(st); err != nil {
		return fmt.Errorf("encode state file: %w", err)
	}

	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync temp state file: %w", err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("close temp state file: %w", err)
	}

	return nil
}

// syncDir opens dir and syncs it, so a rename into it is durable across a crash (research R11).
func syncDir(dir string) error {
	d, err := os.Open(filepath.Clean(dir))
	if err != nil {
		return fmt.Errorf("open state directory: %w", err)
	}
	defer func() { _ = d.Close() }()

	if err = d.Sync(); err != nil {
		return fmt.Errorf("sync state directory: %w", err)
	}

	return nil
}

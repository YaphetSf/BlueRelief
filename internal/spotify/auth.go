// Package spotify is the BlueRelief control-link: OAuth (PKCE) + Spotify Web API.
//
// The audio link (raspotify/librespot → state.json) is untouched. This package
// is the *second* link, the one that lets the GUI send commands back to
// Spotify. It is read by bluerelief-web (control endpoints) and written by
// bluerelief-auth (one-shot PKCE login on the Mac).
package spotify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Scopes is the canonical set of scopes bluerelief-auth requests. Kept in one
// place so the CLI and any future re-auth flow ask for exactly the same
// permissions, which keeps the stored refresh token usable.
var Scopes = []string{
	"user-read-playback-state",
	"user-modify-playback-state",
	"user-read-currently-playing",
	"playlist-read-private",
	"playlist-read-collaborative",
	"user-library-read",
	"user-read-recently-played",
}

const (
	TokenURL      = "https://accounts.spotify.com/api/token"
	AuthorizeURL  = "https://accounts.spotify.com/authorize"
	refreshBuffer = 60 * time.Second
)

// Token is the on-disk shape of /var/lib/BlueRelief/spotify-token.json.
// AccessToken expires; RefreshToken is long-lived and what we actually need
// to persist across reboots.
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type"`
	Scope        string    `json:"scope"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// Valid reports whether the cached access token can still be used without
// refreshing (accounting for the refreshBuffer slack).
func (t Token) Valid() bool {
	return t.AccessToken != "" && time.Now().Add(refreshBuffer).Before(t.ExpiresAt)
}

// Manager is the runtime token holder used by bluerelief-web. It loads the token
// file at startup, refreshes the access token in-place when it's about to
// expire, and atomically writes the refreshed token back so a reboot doesn't
// lose progress.
//
// Concurrency: AccessToken() is the only hot path; it takes a single mutex
// for the (rare) refresh and releases it as soon as a valid token is cached.
type Manager struct {
	clientID string
	path     string
	client   *http.Client

	mu    sync.Mutex
	token Token
}

// NewManager returns a Manager but does not touch disk. Call Load to read the
// token file; without it AccessToken returns ErrNoToken.
func NewManager(clientID, path string) *Manager {
	return &Manager{
		clientID: clientID,
		path:     path,
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

// ErrNoToken means no token file has been loaded yet — the user needs to run
// bluerelief-auth on the Mac and copy the result to the board.
var ErrNoToken = errors.New("spotify: no token loaded — run bluerelief-auth")

// Load reads the token file. It tolerates a missing file (returns ErrNoToken)
// so bluerelief-web can start unauthenticated and surface "needs auth" in the UI.
func (m *Manager) Load() error {
	data, err := os.ReadFile(m.path)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrNoToken
		}
		return fmt.Errorf("read token: %w", err)
	}
	var tok Token
	if err := json.Unmarshal(data, &tok); err != nil {
		return fmt.Errorf("parse token: %w", err)
	}
	if tok.RefreshToken == "" {
		return errors.New("spotify: token file has no refresh_token")
	}
	m.mu.Lock()
	m.token = tok
	m.mu.Unlock()
	return nil
}

// Authorized reports whether a refresh token is loaded. It does NOT check
// expiry — the access token may be stale; that's AccessToken's job.
func (m *Manager) Authorized() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.token.RefreshToken != ""
}

// AccessToken returns a non-expired access token, refreshing if needed. The
// caller does not need to know whether a refresh happened — only that the
// returned string is good for at least refreshBuffer more seconds.
func (m *Manager) AccessToken(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.token.RefreshToken == "" {
		return "", ErrNoToken
	}
	if m.token.Valid() {
		return m.token.AccessToken, nil
	}
	if err := m.refreshLocked(ctx); err != nil {
		return "", err
	}
	return m.token.AccessToken, nil
}

// SetToken replaces the cached token (used by bluerelief-auth after a fresh
// login) and writes it through to disk.
func (m *Manager) SetToken(tok Token) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.token = tok
	return m.writeLocked()
}

// refreshLocked exchanges the refresh token for a fresh access token. Caller
// holds m.mu. Spotify *may* return a new refresh_token; if it does we use it,
// otherwise we keep the old one (the API contract).
func (m *Manager) refreshLocked(ctx context.Context) error {
	if m.clientID == "" {
		return errors.New("spotify: client_id not configured")
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", m.token.RefreshToken)
	form.Set("client_id", m.clientID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("refresh: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("refresh: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var payload struct {
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		Scope        string `json:"scope"`
		ExpiresIn    int    `json:"expires_in"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("refresh decode: %w", err)
	}

	m.token.AccessToken = payload.AccessToken
	m.token.TokenType = payload.TokenType
	if payload.Scope != "" {
		m.token.Scope = payload.Scope
	}
	if payload.RefreshToken != "" {
		m.token.RefreshToken = payload.RefreshToken
	}
	m.token.ExpiresAt = time.Now().Add(time.Duration(payload.ExpiresIn) * time.Second)

	return m.writeLocked()
}

// writeLocked persists the token atomically. Caller holds m.mu.
//
// Atomic write: temp file in the same directory + rename. Same pattern as
// bin/spotify-event uses for state.json — readers (bluerelief-auth on the Mac, a
// future second reader on the board) never see a half-written file.
func (m *Manager) writeLocked() error {
	data, err := json.MarshalIndent(m.token, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(m.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir token dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".spotify-token-*.json")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, m.path)
}

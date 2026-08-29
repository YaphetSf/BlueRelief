package spotify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const apiBase = "https://api.spotify.com/v1"

// Sentinels that callers (the HTTP layer in bluerelief-web) map to user-facing
// statuses. The control link is shallow: anything that isn't one of these is
// just surfaced as a generic 502/500 with the upstream body.
var (
	ErrNoActiveDevice = errors.New("spotify: no active device — start playing on the appliance first")
	ErrForbidden      = errors.New("spotify: forbidden (Premium required, or scope missing)")
	ErrRateLimited    = errors.New("spotify: rate limited")
)

// Client is a thin Web API wrapper. It is *not* a general-purpose SDK — it
// owns only the endpoints BlueRelief actually uses (player + a couple of browse
// reads), because that keeps the error surface and the test surface small.
//
// Device resolution is opportunistic: when a control call would otherwise
// return 404 "no active device", we look up the librespot device by name and
// retry with an explicit device_id. The result is cached so the lookup costs
// at most one extra round trip per cold start.
type Client struct {
	auth       *Manager
	deviceName string
	http       *http.Client

	mu       sync.Mutex
	deviceID string
}

func NewClient(auth *Manager, deviceName string) *Client {
	return &Client{
		auth:       auth,
		deviceName: deviceName,
		http:       &http.Client{Timeout: 8 * time.Second},
	}
}

// DeviceName is the librespot device name we'll target (matches
// LIBRESPOT_DEVICE_NAME on the board, e.g. "BlueRelief" or the hostname).
func (c *Client) DeviceName() string { return c.deviceName }

// ── Player ──────────────────────────────────────────────────────────────

// Play resumes playback. If contextURI is non-empty (album/playlist URI) it
// starts that context from the beginning. trackURIs is an optional list to
// queue/play — leave nil for "just resume".
func (c *Client) Play(ctx context.Context, contextURI string, trackURIs []string) error {
	body := map[string]any{}
	if contextURI != "" {
		body["context_uri"] = contextURI
	}
	if len(trackURIs) > 0 {
		body["uris"] = trackURIs
	}
	return c.playerWrite(ctx, http.MethodPut, "/me/player/play", body)
}

func (c *Client) Pause(ctx context.Context) error {
	return c.playerWrite(ctx, http.MethodPut, "/me/player/pause", nil)
}

func (c *Client) Next(ctx context.Context) error {
	return c.playerWrite(ctx, http.MethodPost, "/me/player/next", nil)
}

func (c *Client) Previous(ctx context.Context) error {
	return c.playerWrite(ctx, http.MethodPost, "/me/player/previous", nil)
}

func (c *Client) Seek(ctx context.Context, positionMS int) error {
	if positionMS < 0 {
		positionMS = 0
	}
	return c.playerWrite(ctx, http.MethodPut, "/me/player/seek?position_ms="+strconv.Itoa(positionMS), nil)
}

func (c *Client) Volume(ctx context.Context, percent int) error {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	return c.playerWrite(ctx, http.MethodPut, "/me/player/volume?volume_percent="+strconv.Itoa(percent), nil)
}

func (c *Client) Shuffle(ctx context.Context, on bool) error {
	return c.playerWrite(ctx, http.MethodPut, "/me/player/shuffle?state="+strconv.FormatBool(on), nil)
}

// Repeat state is one of "off", "context", "track" (Spotify's vocabulary).
func (c *Client) Repeat(ctx context.Context, state string) error {
	switch state {
	case "off", "context", "track":
	default:
		return fmt.Errorf("spotify: invalid repeat state %q", state)
	}
	return c.playerWrite(ctx, http.MethodPut, "/me/player/repeat?state="+state, nil)
}

// Transfer hands the active playback session to deviceID. We use this when a
// control call returned ErrNoActiveDevice — Spotify needs a session "owner"
// before play/pause apply.
func (c *Client) Transfer(ctx context.Context, deviceID string, play bool) error {
	body := map[string]any{
		"device_ids": []string{deviceID},
		"play":       play,
	}
	_, err := c.do(ctx, http.MethodPut, "/me/player", body, nil)
	return err
}

// playerWrite is the shared body for play/pause/next/previous/seek/volume/etc.
// It transparently handles "no active device" by transferring to our librespot
// device and retrying once.
func (c *Client) playerWrite(ctx context.Context, method, path string, body any) error {
	_, err := c.do(ctx, method, path, body, nil)
	if errors.Is(err, ErrNoActiveDevice) {
		dev, derr := c.resolveDevice(ctx)
		if derr != nil {
			return fmt.Errorf("no active device, and: %w", derr)
		}
		// Append device_id to the query, then re-issue.
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		// Transfer first (without auto-play); the original command then takes
		// effect on the now-active device.
		if terr := c.Transfer(ctx, dev, false); terr != nil {
			return fmt.Errorf("transfer to %s: %w", c.deviceName, terr)
		}
		_, err = c.do(ctx, method, path+sep+"device_id="+url.QueryEscape(dev), body, nil)
	}
	return err
}

// ── Devices / Browse ────────────────────────────────────────────────────

type Device struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	IsActive bool   `json:"is_active"`
	Volume   int    `json:"volume_percent"`
}

func (c *Client) Devices(ctx context.Context) ([]Device, error) {
	var resp struct {
		Devices []Device `json:"devices"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/me/player/devices", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Devices, nil
}

// resolveDevice finds the librespot device by name (case-insensitive) and
// caches its id. The cache is invalidated implicitly: if the cached id stops
// working (librespot restart usually rotates it), the next call will 404
// again and we re-resolve.
func (c *Client) resolveDevice(ctx context.Context) (string, error) {
	c.mu.Lock()
	cached := c.deviceID
	c.mu.Unlock()
	if cached != "" {
		return cached, nil
	}
	devices, err := c.Devices(ctx)
	if err != nil {
		return "", err
	}
	if c.deviceName == "" {
		return "", errors.New("spotify: no device name configured (SPOTIFY_DEVICE_NAME)")
	}
	for _, d := range devices {
		if strings.EqualFold(d.Name, c.deviceName) {
			c.mu.Lock()
			c.deviceID = d.ID
			c.mu.Unlock()
			return d.ID, nil
		}
	}
	return "", fmt.Errorf("spotify: device %q not found among %d available devices — is raspotify running?", c.deviceName, len(devices))
}

// Playlist is the trimmed shape we send to the UI. We deliberately drop most
// of Spotify's playlist payload to keep `/api/library/playlists` small enough
// for a 10" touch list.
type Playlist struct {
	URI    string `json:"uri"`
	Name   string `json:"name"`
	Owner  string `json:"owner"`
	Image  string `json:"image"`
	Tracks int    `json:"tracks"`
}

func (c *Client) Playlists(ctx context.Context, limit int) ([]Playlist, error) {
	if limit <= 0 {
		limit = 50
	}
	var raw struct {
		Items []struct {
			URI    string `json:"uri"`
			Name   string `json:"name"`
			Images []struct {
				URL string `json:"url"`
			} `json:"images"`
			Owner struct {
				DisplayName string `json:"display_name"`
				ID          string `json:"id"`
			} `json:"owner"`
			Tracks struct {
				Total int `json:"total"`
			} `json:"items"`
		} `json:"items"`
	}
	path := fmt.Sprintf("/me/playlists?limit=%d", limit)
	if _, err := c.do(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Playlist, 0, len(raw.Items))
	for _, item := range raw.Items {
		owner := item.Owner.DisplayName
		if owner == "" {
			owner = item.Owner.ID
		}
		img := ""
		if len(item.Images) > 0 {
			img = item.Images[0].URL
		}
		out = append(out, Playlist{
			URI:    item.URI,
			Name:   item.Name,
			Owner:  owner,
			Image:  img,
			Tracks: item.Tracks.Total,
		})
	}
	return out, nil
}

// RecentTrack is a flat view over /me/player/recently-played. The web UI
// groups by context (album/playlist) to make the "tap to resume" target
// bigger; that grouping is the front end's job.
type RecentTrack struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Artists     string `json:"artists"`
	Album       string `json:"album"`
	Image       string `json:"image"`
	ContextURI  string `json:"context_uri,omitempty"`
	ContextType string `json:"context_type,omitempty"`
	PlayedAt    string `json:"played_at"`
}

func (c *Client) RecentlyPlayed(ctx context.Context, limit int) ([]RecentTrack, error) {
	if limit <= 0 {
		limit = 25
	}
	var raw struct {
		Items []struct {
			PlayedAt string `json:"played_at"`
			Track    struct {
				URI     string `json:"uri"`
				Name    string `json:"name"`
				Artists []struct {
					Name string `json:"name"`
				} `json:"artists"`
				Album struct {
					Name   string `json:"name"`
					Images []struct {
						URL string `json:"url"`
					} `json:"images"`
				} `json:"album"`
			} `json:"track"`
			Context *struct {
				URI  string `json:"uri"`
				Type string `json:"type"`
			} `json:"context"`
		} `json:"items"`
	}
	path := fmt.Sprintf("/me/player/recently-played?limit=%d", limit)
	if _, err := c.do(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]RecentTrack, 0, len(raw.Items))
	for _, item := range raw.Items {
		artists := make([]string, 0, len(item.Track.Artists))
		for _, a := range item.Track.Artists {
			artists = append(artists, a.Name)
		}
		img := ""
		if len(item.Track.Album.Images) > 0 {
			img = item.Track.Album.Images[0].URL
		}
		rt := RecentTrack{
			URI:      item.Track.URI,
			Name:     item.Track.Name,
			Artists:  strings.Join(artists, ", "),
			Album:    item.Track.Album.Name,
			Image:    img,
			PlayedAt: item.PlayedAt,
		}
		if item.Context != nil {
			rt.ContextURI = item.Context.URI
			rt.ContextType = item.Context.Type
		}
		out = append(out, rt)
	}
	return out, nil
}

// ── Transport ───────────────────────────────────────────────────────────

// do is the single point that signs requests, retries on 401, and converts
// status codes into the sentinel errors above. Keeping it as the only HTTP
// path means there is one place to instrument or change auth handling.
func (c *Client) do(ctx context.Context, method, path string, body any, out any) (*http.Response, error) {
	resp, err := c.send(ctx, method, path, body, false)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		// Force a refresh and retry exactly once. Spotify can revoke tokens
		// out-of-band, so a 401 isn't always "expired" — but a single retry
		// is cheap and disambiguates expiry from revocation.
		resp.Body.Close()
		resp, err = c.send(ctx, method, path, body, true)
		if err != nil {
			return nil, err
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out != nil && resp.StatusCode != http.StatusNoContent {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil && err != io.EOF {
				return resp, fmt.Errorf("decode %s: %w", path, err)
			}
		}
		return resp, nil
	}

	bodyBytes, _ := io.ReadAll(resp.Body)
	bodyText := strings.TrimSpace(string(bodyBytes))

	switch resp.StatusCode {
	case http.StatusNotFound:
		// Spotify returns 404 for "no active device" — distinguish from a
		// real 404 (e.g. wrong endpoint) by sniffing the reason in the body.
		if strings.Contains(strings.ToLower(bodyText), "no active device") {
			return resp, ErrNoActiveDevice
		}
		return resp, fmt.Errorf("404 %s: %s", path, bodyText)
	case http.StatusForbidden:
		return resp, ErrForbidden
	case http.StatusTooManyRequests:
		return resp, ErrRateLimited
	}
	return resp, fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, bodyText)
}

func (c *Client) send(ctx context.Context, method, path string, body any, forceRefresh bool) (*http.Response, error) {
	if forceRefresh {
		// Expire the cache so AccessToken triggers a refresh.
		c.auth.mu.Lock()
		c.auth.token.ExpiresAt = time.Now().Add(-time.Minute)
		c.auth.mu.Unlock()
	}
	token, err := c.auth.AccessToken(ctx)
	if err != nil {
		return nil, err
	}

	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.http.Do(req)
}

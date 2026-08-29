// Package lyrics fetches synced lyrics from LRCLIB (https://lrclib.net) and
// merges them into the state payload that bluerelief-web publishes over SSE.
//
// LRCLIB is keyless and CC-0; matches by (track, artist, album, duration).
// We cache hits *and* misses in memory keyed by Spotify track ID so a single
// session never re-queries for the same song.
package lyrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const defaultBaseURL = "https://lrclib.net"

// ErrNotFound is returned by Client.Get when LRCLIB has no entry for the query.
var ErrNotFound = errors.New("lrclib: no match")

type Client struct {
	BaseURL    string
	HTTPClient *http.Client
	UserAgent  string
}

func NewClient() *Client {
	return &Client{
		BaseURL:    defaultBaseURL,
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
		UserAgent:  "BlueRelief/0.1 (https://github.com/YaphetSf/BlueRelief)",
	}
}

// apiResponse mirrors the LRCLIB /api/get JSON. Only the fields we care about.
type apiResponse struct {
	ID           int64   `json:"id"`
	TrackName    string  `json:"trackName"`
	ArtistName   string  `json:"artistName"`
	AlbumName    string  `json:"albumName"`
	Duration     float64 `json:"duration"`
	Instrumental bool    `json:"instrumental"`
	PlainLyrics  string  `json:"plainLyrics"`
	SyncedLyrics string  `json:"syncedLyrics"`
}

// Get queries LRCLIB /api/get for an exact match. durationSec may be 0 to
// omit; LRCLIB tolerates a missing duration but matches are noisier.
func (c *Client) Get(ctx context.Context, track, artist, album string, durationSec int) (*apiResponse, error) {
	q := url.Values{}
	q.Set("track_name", track)
	q.Set("artist_name", artist)
	if album != "" {
		q.Set("album_name", album)
	}
	if durationSec > 0 {
		q.Set("duration", strconv.Itoa(durationSec))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/get?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var r apiResponse
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			return nil, fmt.Errorf("lrclib: decode: %w", err)
		}
		return &r, nil
	case http.StatusNotFound:
		return nil, ErrNotFound
	default:
		return nil, fmt.Errorf("lrclib: status %d", resp.StatusCode)
	}
}

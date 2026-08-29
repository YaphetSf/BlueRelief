package lyrics

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"
)

// Payload matches the JSON shape the web UI reads from state.lyrics
// (see fixtures/state-with-lyrics.json).
type Payload struct {
	Source string `json:"source"`
	Synced bool   `json:"synced"`
	Lines  []Line `json:"lines"`
}

// trackInfo is the minimum slice of state.json the fetcher needs.
type trackInfo struct {
	Track *struct {
		ID         string   `json:"id"`
		Name       string   `json:"name"`
		Artists    []string `json:"artists"`
		Album      string   `json:"album"`
		DurationMS *int     `json:"duration_ms"`
	} `json:"track"`
	Lyrics json.RawMessage `json:"lyrics"`
}

type cacheEntry struct {
	payload *Payload // nil = confirmed miss; keep so we don't re-query
	at      time.Time
}

// Fetcher injects LRCLIB-sourced lyrics into state payloads as they pass
// through bluerelief-web's pollLoop.
//
// Concurrency model:
//   - Process is called from the polling goroutine; it returns *immediately*
//     with either the original payload or an enriched copy (cache hit).
//   - A background goroutine handles the network round-trip and, when it
//     completes, republishes the *most recent* payload via Publish.
//
// The "republish latest" trick keeps the SSE stream snappy: if a position
// update arrived during the LRCLIB call, that newer position rides along with
// the freshly fetched lyrics.
type Fetcher struct {
	client  *Client
	publish func([]byte)

	mu        sync.Mutex
	cache     map[string]cacheEntry
	pending   map[string]bool
	latestRaw []byte
	latestKey string
}

func NewFetcher(client *Client, publish func([]byte)) *Fetcher {
	return &Fetcher{
		client:  client,
		publish: publish,
		cache:   map[string]cacheEntry{},
		pending: map[string]bool{},
	}
}

// Process enriches raw with lyrics if we have them, and kicks off a
// background fetch otherwise. Returning the (possibly mutated) payload is the
// caller's cue to publish; the async path republishes through f.publish once
// the LRCLIB call completes.
func (f *Fetcher) Process(raw []byte) []byte {
	var info trackInfo
	if err := json.Unmarshal(raw, &info); err != nil || info.Track == nil {
		return raw
	}
	// Honor pre-baked lyrics (fixtures use this).
	if len(info.Lyrics) > 0 && string(info.Lyrics) != "null" {
		return raw
	}

	key := info.Track.ID
	if key == "" || info.Track.Name == "" {
		return raw
	}

	f.mu.Lock()
	f.latestRaw = raw
	f.latestKey = key
	entry, hit := f.cache[key]
	pending := f.pending[key]
	if !hit && !pending {
		f.pending[key] = true
	}
	f.mu.Unlock()

	if hit {
		if entry.payload == nil {
			return raw
		}
		return injectLyrics(raw, entry.payload)
	}

	if !pending {
		artist := ""
		if len(info.Track.Artists) > 0 {
			artist = info.Track.Artists[0]
		}
		dur := 0
		if info.Track.DurationMS != nil {
			dur = (*info.Track.DurationMS + 500) / 1000
		}
		go f.fetch(key, info.Track.Name, artist, info.Track.Album, dur)
	}

	return raw
}

func (f *Fetcher) fetch(key, name, artist, album string, durationSec int) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	start := time.Now()
	resp, err := f.client.Get(ctx, name, artist, album, durationSec)
	elapsed := time.Since(start)

	var payload *Payload
	switch {
	case err == nil && resp != nil && resp.SyncedLyrics != "":
		lines := ParseLRC(resp.SyncedLyrics)
		if len(lines) > 0 {
			payload = &Payload{Source: "lrclib", Synced: true, Lines: lines}
			log.Printf("bluerelief-web: lyrics %q / %q: %d lines (%v)", name, artist, len(lines), elapsed.Round(time.Millisecond))
		} else {
			log.Printf("bluerelief-web: lyrics %q / %q: hit but no synced lines (%v)", name, artist, elapsed.Round(time.Millisecond))
		}
	case errors.Is(err, ErrNotFound):
		log.Printf("bluerelief-web: lyrics %q / %q: not found (%v)", name, artist, elapsed.Round(time.Millisecond))
	case err != nil:
		log.Printf("bluerelief-web: lyrics %q / %q: %v (%v)", name, artist, err, elapsed.Round(time.Millisecond))
	}

	f.mu.Lock()
	delete(f.pending, key)
	f.cache[key] = cacheEntry{payload: payload, at: time.Now()}
	raw := f.latestRaw
	sameTrack := f.latestKey == key
	f.mu.Unlock()

	if !sameTrack || raw == nil || payload == nil {
		return
	}
	f.publish(injectLyrics(raw, payload))
}

// injectLyrics returns a new JSON payload identical to raw but with the
// top-level "lyrics" key set to p. We round-trip through map[string]any
// because state.json keys aren't ordered (Go marshals maps alphabetically)
// and byte splicing is fragile.
func injectLyrics(raw []byte, p *Payload) []byte {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return raw
	}
	encoded, err := json.Marshal(p)
	if err != nil {
		return raw
	}
	doc["lyrics"] = encoded
	out, err := json.Marshal(doc)
	if err != nil {
		return raw
	}
	return out
}

package lyrics

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return fn(r)
}

func testClient(fn roundTripFunc) *Client {
	return &Client{
		BaseURL: "https://lyrics.test",
		HTTPClient: &http.Client{
			Transport: fn,
		},
		UserAgent: "test",
	}
}

func jsonResponse(status int, body any) *http.Response {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(&buf),
	}
}

func TestFetcher_PreservesBakedLyrics(t *testing.T) {
	raw := []byte(`{"track":{"id":"x","name":"n","artists":["a"],"duration_ms":1000},"lyrics":{"source":"demo","synced":true,"lines":[{"time_ms":0,"text":"y"}]}}`)

	c := testClient(func(r *http.Request) (*http.Response, error) {
		t.Errorf("LRCLIB must not be called when lyrics are pre-baked; hit %s", r.URL)
		return jsonResponse(http.StatusNotFound, nil), nil
	})

	f := NewFetcher(c, func([]byte) {})
	out := f.Process(raw)
	if string(out) != string(raw) {
		t.Errorf("expected raw unchanged, got %s", out)
	}
}

func TestFetcher_FetchesAndRepublishes(t *testing.T) {
	raw := []byte(`{"track":{"id":"abc","name":"Dani California","artists":["Red Hot Chili Peppers"],"album":"Stadium Arcadium","duration_ms":282160}}`)

	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, apiResponse{
			TrackName:    "Dani California",
			ArtistName:   "Red Hot Chili Peppers",
			SyncedLyrics: "[00:14.06]Getting born\n[00:18.50]Papa was a copper\n",
		}), nil
	})

	var (
		wg        sync.WaitGroup
		published []byte
	)
	wg.Add(1)
	publish := func(b []byte) {
		published = b
		wg.Done()
	}

	f := NewFetcher(c, publish)

	// First call: no cache, returns raw, kicks off async fetch.
	first := f.Process(raw)
	if string(first) != string(raw) {
		t.Errorf("first call should return raw, got %s", first)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publish never called")
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(published, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["lyrics"]; !ok {
		t.Fatalf("published payload missing lyrics: %s", published)
	}

	// Second call should hit the cache (synchronous inject).
	second := f.Process(raw)
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(second, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["lyrics"]; !ok {
		t.Errorf("cache hit didn't inject lyrics: %s", second)
	}
}

func TestFetcher_CachesMisses(t *testing.T) {
	raw := []byte(`{"track":{"id":"missing","name":"Ghost","artists":["Nobody"],"duration_ms":1000}}`)

	var calls int
	var mu sync.Mutex
	c := testClient(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return jsonResponse(http.StatusNotFound, nil), nil
	})

	f := NewFetcher(c, func([]byte) {})

	f.Process(raw)
	// Wait for the in-flight fetch to settle.
	for i := 0; i < 50; i++ {
		f.mu.Lock()
		pending := len(f.pending)
		f.mu.Unlock()
		if pending == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Subsequent calls must not re-hit LRCLIB.
	for i := 0; i < 3; i++ {
		f.Process(raw)
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("expected 1 LRCLIB call, got %d", calls)
	}
}

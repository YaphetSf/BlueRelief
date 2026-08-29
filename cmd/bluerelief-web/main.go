package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"bluerelief/internal/lyrics"
	"bluerelief/internal/spotify"
	bluereliefState "bluerelief/internal/state"
	"bluerelief/internal/web"
)

const (
	defaultAddr            = ":8085"
	defaultState           = "/run/BlueRelief/state.json"
	defaultMockPath        = "fixtures/state-playing.json"
	defaultTokenPath       = "/var/lib/BlueRelief/spotify-token.json"
	defaultAirplayArtwork  = "/run/BlueRelief/airplay-artwork"
	defaultAirplayMPRISBus = "org.mpris.MediaPlayer2.ShairportSync"
	defaultAirplayPlayer   = "shairport-sync"
	pollInterval           = 500 * time.Millisecond
)

type broker struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
	latest  []byte
}

func newBroker() *broker {
	return &broker{clients: map[chan []byte]struct{}{}}
}

func (b *broker) snapshot() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.latest
}

func (b *broker) publish(payload []byte) {
	b.mu.Lock()
	if bytes.Equal(b.latest, payload) {
		b.mu.Unlock()
		return
	}
	b.latest = payload
	clients := make([]chan []byte, 0, len(b.clients))
	for c := range b.clients {
		clients = append(clients, c)
	}
	b.mu.Unlock()

	for _, c := range clients {
		select {
		case c <- payload:
		default:
		}
	}
}

func (b *broker) subscribe() chan []byte {
	c := make(chan []byte, 4)
	b.mu.Lock()
	b.clients[c] = struct{}{}
	latest := b.latest
	b.mu.Unlock()
	if latest != nil {
		c <- latest
	}
	return c
}

func (b *broker) unsubscribe(c chan []byte) {
	b.mu.Lock()
	delete(b.clients, c)
	b.mu.Unlock()
}

func envDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

// envEnabled reads one of bluerelief.conf's on|off switches out of the
// environment systemd built from it. Anything we don't recognise — including
// an unset key — reads as enabled, matching bluerelief-config's fail-open rule.
func envEnabled(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "off", "0", "false", "no":
		return false
	default:
		return true
	}
}

func main() {
	var (
		addr               string
		statePath          string
		mock               bool
		mockPath           string
		clientID           string
		deviceName         string
		tokenPath          string
		noLyrics           bool
		airplayArtworkPath string
		airplayControlMode string
		airplayMPRISBus    string
		airplayPlayer      string
	)
	flag.StringVar(&addr, "addr", defaultAddr, "HTTP listen address")
	flag.StringVar(&statePath, "state", defaultState, "path to BlueRelief state.json")
	flag.BoolVar(&mock, "mock", false, "serve a static fixture instead of polling --state")
	flag.StringVar(&mockPath, "mock-path", defaultMockPath, "fixture path used when --mock is set")
	flag.StringVar(&clientID, "spotify-client-id", os.Getenv("SPOTIFY_CLIENT_ID"), "Spotify app Client ID (config: SPOTIFY_CLIENT_ID)")
	flag.StringVar(&deviceName, "spotify-device-name", os.Getenv("SPOTIFY_DEVICE_NAME"), "librespot device name to target (config: SPOTIFY_DEVICE_NAME)")
	flag.StringVar(&tokenPath, "spotify-token", defaultTokenPath, "path to spotify-token.json")
	flag.BoolVar(&noLyrics, "no-lyrics", !envEnabled("LYRICS"), "disable LRCLIB synced-lyrics fetching (config: LYRICS=off)")
	flag.StringVar(&airplayArtworkPath, "airplay-artwork", defaultAirplayArtwork, "path to latest AirPlay artwork")
	flag.StringVar(&airplayControlMode, "airplay-control-mode", envDefault("AIRPLAY_CONTROL", "mpris"), "AirPlay control backend: mpris, playerctl, or none (config: AIRPLAY_CONTROL)")
	flag.StringVar(&airplayMPRISBus, "airplay-mpris-bus-name", defaultAirplayMPRISBus, "MPRIS bus name for shairport-sync control")
	flag.StringVar(&airplayPlayer, "airplay-player", defaultAirplayPlayer, "playerctl player name for shairport-sync control")
	flag.Parse()

	br := newBroker()

	var fetcher *lyrics.Fetcher
	if !noLyrics {
		fetcher = lyrics.NewFetcher(lyrics.NewClient(), br.publish)
	}
	process := func(b []byte) []byte {
		if fetcher == nil {
			return b
		}
		return fetcher.Process(b)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	source := statePath
	if mock {
		source = mockPath
		if err := loadMock(mockPath, br, process); err != nil {
			log.Fatalf("bluerelief-web: mock: %v", err)
		}
	} else {
		go pollLoop(ctx, statePath, br, process)
	}

	assets, err := fs.Sub(web.Assets, "assets")
	if err != nil {
		log.Fatalf("bluerelief-web: embed: %v", err)
	}

	// Spotify control link (separate from the audio link). Optional: if the
	// token file is missing the server still serves the read-only UI; the
	// control endpoints just answer 503 "needs auth" until bluerelief-auth runs.
	var spotifyClient *spotify.Client
	if clientID != "" {
		mgr := spotify.NewManager(clientID, tokenPath)
		switch err := mgr.Load(); {
		case err == nil:
			spotifyClient = spotify.NewClient(mgr, deviceName)
			log.Printf("bluerelief-web: spotify control enabled (device=%q, token=%s)", deviceName, tokenPath)
		case errors.Is(err, spotify.ErrNoToken):
			log.Printf("bluerelief-web: spotify control disabled — no token at %s (run bluerelief-auth)", tokenPath)
		default:
			log.Printf("bluerelief-web: spotify control disabled — %v", err)
		}
	} else {
		log.Printf("bluerelief-web: spotify control disabled — SPOTIFY_CLIENT_ID not set")
	}

	airplayClient := newAirplayClient(airplayControlMode, airplayMPRISBus, airplayPlayer)
	if airplayClient.mode == "none" {
		log.Printf("bluerelief-web: airplay control disabled")
	} else {
		log.Printf("bluerelief-web: airplay control enabled (mode=%s)", airplayClient.mode)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		payload := br.snapshot()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if payload == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"no state yet"}`))
			return
		}
		_, _ = w.Write(payload)
	})

	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		ch := br.subscribe()
		defer br.unsubscribe(ch)

		ping := time.NewTicker(15 * time.Second)
		defer ping.Stop()

		for {
			select {
			case <-r.Context().Done():
				return
			case payload, ok := <-ch:
				if !ok {
					return
				}
				if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
					return
				}
				flusher.Flush()
			case <-ping.C:
				if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	})

	// ── Provider-routed control link ─────────────────────────────────────
	// Spotify keeps the full Web API path; AirPlay exposes only the current
	// session controls that shairport-sync can surface through MPRIS/playerctl.
	ctl := &controlAPI{spotify: spotifyClient, airplay: airplayClient, states: br}

	mux.HandleFunc("/api/auth/status", ctl.status)
	mux.HandleFunc("/api/control/play", ctl.wrap("transport", func(ctx context.Context, r *http.Request, provider string) error {
		switch provider {
		case "airplay":
			return ctl.airplay.Play(ctx)
		default:
			client, err := ctl.requireSpotify()
			if err != nil {
				return err
			}
			return client.Play(ctx, "", nil)
		}
	}))
	mux.HandleFunc("/api/control/pause", ctl.wrap("transport", func(ctx context.Context, r *http.Request, provider string) error {
		switch provider {
		case "airplay":
			return ctl.airplay.Pause(ctx)
		default:
			client, err := ctl.requireSpotify()
			if err != nil {
				return err
			}
			return client.Pause(ctx)
		}
	}))
	mux.HandleFunc("/api/control/next", ctl.wrap("transport", func(ctx context.Context, r *http.Request, provider string) error {
		switch provider {
		case "airplay":
			return ctl.airplay.Next(ctx)
		default:
			client, err := ctl.requireSpotify()
			if err != nil {
				return err
			}
			return client.Next(ctx)
		}
	}))
	mux.HandleFunc("/api/control/previous", ctl.wrap("transport", func(ctx context.Context, r *http.Request, provider string) error {
		switch provider {
		case "airplay":
			return ctl.airplay.Previous(ctx)
		default:
			client, err := ctl.requireSpotify()
			if err != nil {
				return err
			}
			return client.Previous(ctx)
		}
	}))
	mux.HandleFunc("/api/control/seek", ctl.wrap("seek", func(ctx context.Context, r *http.Request, provider string) error {
		ms, err := strconv.Atoi(r.URL.Query().Get("ms"))
		if err != nil {
			return &badRequestErr{"seek: ms must be an integer"}
		}
		if provider == "airplay" {
			return &unsupportedErr{"airplay seek is not supported"}
		}
		client, err := ctl.requireSpotify()
		if err != nil {
			return err
		}
		return client.Seek(ctx, ms)
	}))
	mux.HandleFunc("/api/control/volume", ctl.wrap("volume", func(ctx context.Context, r *http.Request, provider string) error {
		pct, err := strconv.Atoi(r.URL.Query().Get("percent"))
		if err != nil {
			return &badRequestErr{"volume: percent must be an integer 0..100"}
		}
		switch provider {
		case "airplay":
			return ctl.airplay.Volume(ctx, pct)
		default:
			client, err := ctl.requireSpotify()
			if err != nil {
				return err
			}
			return client.Volume(ctx, pct)
		}
	}))
	mux.HandleFunc("/api/control/shuffle", ctl.wrapSpotifyOnly("spotify shuffle", func(ctx context.Context, r *http.Request) error {
		var body struct {
			On bool `json:"on"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return &badRequestErr{"shuffle: body must be {\"on\":bool}"}
		}
		client, err := ctl.requireSpotify()
		if err != nil {
			return err
		}
		return client.Shuffle(ctx, body.On)
	}))
	mux.HandleFunc("/api/control/repeat", ctl.wrapSpotifyOnly("spotify repeat", func(ctx context.Context, r *http.Request) error {
		var body struct {
			State string `json:"state"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return &badRequestErr{"repeat: body must be {\"state\":\"off|context|track\"}"}
		}
		client, err := ctl.requireSpotify()
		if err != nil {
			return err
		}
		return client.Repeat(ctx, body.State)
	}))
	mux.HandleFunc("/api/control/play-context", ctl.wrapSpotifyOnly("spotify browse", func(ctx context.Context, r *http.Request) error {
		var body struct {
			ContextURI string   `json:"context_uri"`
			URIs       []string `json:"uris"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return &badRequestErr{"play-context: body must be {\"context_uri\":\"…\"}"}
		}
		if body.ContextURI == "" && len(body.URIs) == 0 {
			return &badRequestErr{"play-context: context_uri or uris required"}
		}
		client, err := ctl.requireSpotify()
		if err != nil {
			return err
		}
		return client.Play(ctx, body.ContextURI, body.URIs)
	}))

	mux.HandleFunc("/api/library/playlists", ctl.wrapSpotifyJSON("spotify browse", func(ctx context.Context, r *http.Request) (any, error) {
		client, err := ctl.requireSpotify()
		if err != nil {
			return nil, err
		}
		return client.Playlists(ctx, 50)
	}))
	mux.HandleFunc("/api/library/recent", ctl.wrapSpotifyJSON("spotify browse", func(ctx context.Context, r *http.Request) (any, error) {
		client, err := ctl.requireSpotify()
		if err != nil {
			return nil, err
		}
		return client.RecentlyPlayed(ctx, 25)
	}))

	mux.HandleFunc("/api/artwork/airplay", serveArtwork(airplayArtworkPath))
	mux.Handle("/assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(assets))))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data, err := fs.ReadFile(assets, "index.html")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(data)
	})

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Printf("bluerelief-web: listening on %s (state=%s mock=%v)", addr, source, mock)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("bluerelief-web: %v", err)
	}
}

func loadMock(path string, br *broker, process func([]byte) []byte) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}

	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	payload["updated_at"] = now
	if playback, ok := payload["playback"].(map[string]any); ok {
		playback["position_updated_at"] = now
	}

	rewritten, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	br.publish(process(rewritten))
	return nil
}

// controlAPI routes control calls to the provider that currently owns
// state.json. Spotify keeps the full Web API path; AirPlay gets the smaller
// MPRIS/playerctl surface available for the current session.
type controlAPI struct {
	spotify *spotify.Client
	airplay *airplayClient
	states  *broker
}

// badRequestErr is a sentinel used by handler closures to ask for 400.
type badRequestErr struct{ msg string }

func (e *badRequestErr) Error() string { return e.msg }

// unsupportedErr is returned when the active provider cannot perform a command.
type unsupportedErr struct{ msg string }

func (e *unsupportedErr) Error() string { return e.msg }

func (c *controlAPI) status(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	resp := map[string]any{
		"authorized":      c.spotify != nil,
		"device_name":     "",
		"airplay_control": c.airplay != nil && c.airplay.mode != "none",
	}
	if c.spotify != nil {
		resp["device_name"] = c.spotify.DeviceName()
	}
	if snapshot, err := c.currentState(); err == nil {
		resp["source"] = snapshot.SourceKind()
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (c *controlAPI) wrap(capability string, fn func(ctx context.Context, r *http.Request, provider string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		provider, err := c.routeProvider(r, capability)
		if err != nil {
			writeControlError(w, err)
			return
		}
		err = fn(r.Context(), r, provider)
		if err == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeControlError(w, err)
	}
}

func (c *controlAPI) wrapSpotifyOnly(name string, fn func(ctx context.Context, r *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		provider, err := c.routeProvider(r, "browse")
		if err != nil {
			writeControlError(w, err)
			return
		}
		if provider != "spotify" {
			writeControlError(w, &unsupportedErr{name + " is not available for AirPlay"})
			return
		}
		err = fn(r.Context(), r)
		if err == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeControlError(w, err)
	}
}

func (c *controlAPI) wrapSpotifyJSON(name string, fn func(ctx context.Context, r *http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		provider, err := c.routeProvider(r, "browse")
		if err != nil {
			writeControlError(w, err)
			return
		}
		if provider != "spotify" {
			writeControlError(w, &unsupportedErr{name + " is not available for AirPlay"})
			return
		}
		data, err := fn(r.Context(), r)
		if err != nil {
			writeControlError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(data)
	}
}

func (c *controlAPI) currentState() (bluereliefState.State, error) {
	payload := c.states.snapshot()
	if payload == nil {
		return bluereliefState.State{}, errors.New("no state yet")
	}
	var snapshot bluereliefState.State
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		return bluereliefState.State{}, fmt.Errorf("state: %w", err)
	}
	return snapshot, nil
}

func (c *controlAPI) routeProvider(r *http.Request, capability string) (string, error) {
	requested := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("source")))
	snapshot, err := c.currentState()
	if requested == "" {
		if err == nil {
			requested = snapshot.SourceKind()
		} else {
			requested = "spotify"
		}
	}
	if requested == "idle" || requested == "none" {
		return "", &unsupportedErr{"no active playback source"}
	}
	if requested != "spotify" && requested != "airplay" {
		return "", &badRequestErr{"source must be spotify or airplay"}
	}
	if err == nil && requested == snapshot.SourceKind() && capability != "" && !snapshot.Can(capability) {
		return "", &unsupportedErr{fmt.Sprintf("%s does not support %s", requested, capability)}
	}
	if requested == "airplay" && (c.airplay == nil || c.airplay.mode == "none") {
		return "", &unsupportedErr{"airplay control is disabled"}
	}
	return requested, nil
}

func (c *controlAPI) requireSpotify() (*spotify.Client, error) {
	if c.spotify == nil {
		return nil, spotify.ErrNoToken
	}
	return c.spotify, nil
}

type airplayClient struct {
	mode    string
	busName string
	player  string
}

func newAirplayClient(mode, busName, player string) *airplayClient {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "mpris"
	}
	return &airplayClient{
		mode:    mode,
		busName: strings.TrimSpace(busName),
		player:  strings.TrimSpace(player),
	}
}

func (a *airplayClient) Play(ctx context.Context) error {
	return a.playerCommand(ctx, "Play", "play")
}

func (a *airplayClient) Pause(ctx context.Context) error {
	return a.playerCommand(ctx, "Pause", "pause")
}

func (a *airplayClient) Next(ctx context.Context) error {
	return a.playerCommand(ctx, "Next", "next")
}

func (a *airplayClient) Previous(ctx context.Context) error {
	return a.playerCommand(ctx, "Previous", "previous")
}

func (a *airplayClient) Volume(ctx context.Context, percent int) error {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	switch a.mode {
	case "none":
		return &unsupportedErr{"airplay control is disabled"}
	case "playerctl":
		return runCommand(ctx, "playerctl", "-p", a.player, "volume", fmt.Sprintf("%.2f", float64(percent)/100))
	case "mpris":
		return runCommand(
			ctx,
			"busctl",
			"--system",
			"call",
			a.busName,
			"/org/mpris/MediaPlayer2",
			"org.mpris.MediaPlayer2.Player",
			"SetVolume",
			"d",
			fmt.Sprintf("%.3f", float64(percent)/100),
		)
	default:
		return &unsupportedErr{fmt.Sprintf("unknown airplay control mode %q", a.mode)}
	}
}

func (a *airplayClient) playerCommand(ctx context.Context, mprisMethod, playerctlCommand string) error {
	switch a.mode {
	case "none":
		return &unsupportedErr{"airplay control is disabled"}
	case "playerctl":
		return runCommand(ctx, "playerctl", "-p", a.player, playerctlCommand)
	case "mpris":
		return runCommand(
			ctx,
			"busctl",
			"--system",
			"call",
			a.busName,
			"/org/mpris/MediaPlayer2",
			"org.mpris.MediaPlayer2.Player",
			mprisMethod,
		)
	default:
		return &unsupportedErr{fmt.Sprintf("unknown airplay control mode %q", a.mode)}
	}
}

func runCommand(ctx context.Context, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s: %s", name, msg)
	}
	return nil
}

func serveArtwork(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		contentType := http.DetectContentType(data)
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(data)
	}
}

func writeControlError(w http.ResponseWriter, err error) {
	var bad *badRequestErr
	var unsupported *unsupportedErr
	switch {
	case errors.As(err, &bad):
		writeJSONError(w, http.StatusBadRequest, bad.msg)
	case errors.As(err, &unsupported):
		writeJSONError(w, http.StatusNotImplemented, unsupported.msg)
	case errors.Is(err, spotify.ErrNoActiveDevice):
		writeJSONError(w, http.StatusConflict, err.Error())
	case errors.Is(err, spotify.ErrForbidden):
		writeJSONError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, spotify.ErrRateLimited):
		writeJSONError(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, spotify.ErrNoToken):
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
	default:
		writeJSONError(w, http.StatusBadGateway, err.Error())
	}
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func pollLoop(ctx context.Context, path string, br *broker, process func([]byte) []byte) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var lastMod time.Time
	var lastSize int64

	read := func() {
		info, err := os.Stat(path)
		if err != nil {
			return
		}
		if info.ModTime().Equal(lastMod) && info.Size() == lastSize {
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return
		}
		lastMod = info.ModTime()
		lastSize = info.Size()

		// state.json is written pretty-printed (multi-line). SSE frames
		// terminate a data: field at the first newline, so a multi-line
		// payload would reach the browser truncated to its first line.
		// Compact to a single line before publishing.
		var compact bytes.Buffer
		if err := json.Compact(&compact, data); err != nil {
			return
		}
		br.publish(process(compact.Bytes()))
	}

	read()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			read()
		}
	}
}

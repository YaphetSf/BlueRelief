// Command bluerelief-screen blanks the kiosk DisplayPort output when nothing
// is playing and wakes it on resume. It subscribes to bluerelief-web's SSE
// stream and drives sway via `swaymsg output <selector> dpms on|off`.
//
// Defaults: connect to http://127.0.0.1:8085/api/events, 5-minute idle
// timeout, target every output (`*`), auto-discover the sway IPC socket
// under $XDG_RUNTIME_DIR. The idle timeout's default comes from SCREEN_IDLE in
// /etc/BlueRelief/bluerelief.conf; SCREEN_IDLE=off (or --idle-timeout 0) means
// never blank.
//
// Note: we use `dpms` (the older subcommand) rather than `power` because
// Debian 12's sway is 1.7 — `output ... power on|off` was added in 1.8.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

type playbackEvent struct {
	Playback struct {
		IsPlaying bool   `json:"is_playing"`
		Status    string `json:"status"`
	} `json:"playback"`
}

func main() {
	var (
		eventsURL   string
		idleTimeout time.Duration
		output      string
		swaySock    string
	)
	flag.StringVar(&eventsURL, "events", "http://127.0.0.1:8085/api/events", "SSE stream from bluerelief-web")
	flag.DurationVar(&idleTimeout, "idle-timeout", screenIdleDefault(), "non-playing time before screen off; 0 = never (config: SCREEN_IDLE)")
	flag.StringVar(&output, "output", "*", "sway output selector (e.g. DP-1 or *)")
	flag.StringVar(&swaySock, "sway-sock", "", "sway IPC socket path (default: auto-discover)")
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	if idleTimeout <= 0 {
		log.Printf("bluerelief-screen: events=%s idle-timeout=never output=%s", eventsURL, output)
	} else {
		log.Printf("bluerelief-screen: events=%s idle-timeout=%s output=%s", eventsURL, idleTimeout, output)
	}
	if err := run(ctx, eventsURL, idleTimeout, output, swaySock); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("bluerelief-screen: %v", err)
	}
}

// screenIdleDefault reads SCREEN_IDLE from the environment systemd built out
// of bluerelief.conf. "off"/"never" (and anything unparseable) mean never
// blank, expressed as a zero duration.
func screenIdleDefault() time.Duration {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("SCREEN_IDLE")))
	switch raw {
	case "":
		return 5 * time.Minute
	case "off", "never", "0":
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		log.Printf("bluerelief-screen: SCREEN_IDLE=%q is not a duration; never blanking", raw)
		return 0
	}
	return d
}

func run(ctx context.Context, eventsURL string, idleTimeout time.Duration, output, swaySock string) error {
	events := make(chan playbackEvent, 8)
	go streamEvents(ctx, eventsURL, events)

	// Idle timer: armed when we see is_playing=false and the screen is on;
	// fires once after idleTimeout and turns the screen off. Start stopped.
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	armed := false
	arm := func() {
		// SCREEN_IDLE=off lands here as a non-positive duration: stay lit.
		if armed || idleTimeout <= 0 {
			return
		}
		timer.Reset(idleTimeout)
		armed = true
	}
	disarm := func() {
		if !armed {
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		armed = false
	}

	lastApplied := "" // "", "on", "off"
	apply := func(state string) {
		if state == lastApplied {
			return
		}
		if err := setPower(swaySock, output, state); err != nil {
			log.Printf("swaymsg dpms %s: %v", state, err)
			return
		}
		log.Printf("screen → %s", state)
		lastApplied = state
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev := <-events:
			if ev.Playback.IsPlaying {
				disarm()
				apply("on")
			} else if lastApplied != "off" {
				arm()
			}
		case <-timer.C:
			armed = false
			apply("off")
		}
	}
}

// streamEvents opens the SSE connection and pushes each event onto out,
// reconnecting with bounded exponential backoff on failure. The first
// frame delivered by the broker is the latest snapshot, so we always
// start with a known playback state.
func streamEvents(ctx context.Context, url string, out chan<- playbackEvent) {
	backoff := time.Second
	for {
		err := streamOnce(ctx, url, out)
		if errors.Is(err, context.Canceled) {
			return
		}
		if err != nil {
			log.Printf("sse: %v (reconnect in %s)", err, backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		} else {
			backoff = 30 * time.Second
		}
	}
}

func streamOnce(ctx context.Context, url string, out chan<- playbackEvent) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := line[len("data: "):]
		var ev playbackEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			log.Printf("sse: bad payload: %v", err)
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case out <- ev:
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return io.EOF
}

func setPower(sockOverride, output, state string) error {
	sock, err := resolveSock(sockOverride)
	if err != nil {
		return err
	}
	cmd := exec.Command("swaymsg", "-s", sock, "output", output, "dpms", state)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// resolveSock returns the sway IPC socket. With an override, returns it
// verbatim. Otherwise picks the newest sway-ipc.*.sock under
// $XDG_RUNTIME_DIR — we re-resolve on every call so a sway restart
// (different PID, new socket) is picked up transparently.
func resolveSock(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	runtime := os.Getenv("XDG_RUNTIME_DIR")
	if runtime == "" {
		return "", errors.New("XDG_RUNTIME_DIR not set; pass --sway-sock")
	}
	matches, err := filepath.Glob(filepath.Join(runtime, "sway-ipc.*.sock"))
	if err != nil {
		return "", err
	}
	type entry struct {
		path string
		mod  time.Time
	}
	var entries []entry
	for _, m := range matches {
		fi, err := os.Stat(m)
		if err != nil {
			continue
		}
		entries = append(entries, entry{m, fi.ModTime()})
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("no sway-ipc socket under %s", runtime)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].mod.After(entries[j].mod)
	})
	return entries[0].path, nil
}

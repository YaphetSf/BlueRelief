// bluerelief-auth is a one-shot CLI that runs on the Mac to perform the initial
// Spotify Authorization Code + PKCE login and write a token file that can be
// scp'd to the ROCK board.
//
// Why on the Mac and not the board:
//
//	The board is a kiosk with no keyboard — there's no way to type a Spotify
//	password into the browser running on the DP screen. So we do the login on
//	a normal machine and ship just the refresh token to the appliance.
//
// Usage:
//
//	export SPOTIFY_CLIENT_ID=xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
//	go run ./cmd/bluerelief-auth -out spotify-token.json
//	scp spotify-token.json Rock:/var/lib/BlueRelief/spotify-token.json
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"bluerelief/internal/spotify"
)

const (
	defaultRedirect = "http://127.0.0.1:8888/callback"
	defaultOut      = "spotify-token.json"
	listenPort      = "8888"
)

func main() {
	var (
		clientID string
		redirect string
		outPath  string
	)
	flag.StringVar(&clientID, "client-id", os.Getenv("SPOTIFY_CLIENT_ID"), "Spotify app Client ID (defaults to $SPOTIFY_CLIENT_ID)")
	flag.StringVar(&redirect, "redirect", defaultRedirect, "OAuth redirect URI (must match an entry in your Spotify app)")
	flag.StringVar(&outPath, "out", defaultOut, "where to write the token JSON")
	flag.Parse()

	if clientID == "" {
		log.Fatal("bluerelief-auth: --client-id (or $SPOTIFY_CLIENT_ID) is required")
	}

	verifier, challenge, err := pkcePair()
	if err != nil {
		log.Fatalf("bluerelief-auth: pkce: %v", err)
	}
	stateNonce, err := randomString(24)
	if err != nil {
		log.Fatalf("bluerelief-auth: state: %v", err)
	}

	authURL := buildAuthURL(clientID, redirect, challenge, stateNonce)

	// One-shot HTTP server on :8888 that accepts the redirect, hands the code
	// back to main via a channel, and exits.
	type callback struct {
		code string
		err  error
	}
	ch := make(chan callback, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if errParam := q.Get("error"); errParam != "" {
			renderResult(w, false, errParam)
			ch <- callback{err: fmt.Errorf("authorization denied: %s", errParam)}
			return
		}
		if q.Get("state") != stateNonce {
			renderResult(w, false, "state nonce mismatch")
			ch <- callback{err: errors.New("state nonce mismatch")}
			return
		}
		code := q.Get("code")
		if code == "" {
			renderResult(w, false, "no code in callback")
			ch <- callback{err: errors.New("callback missing code")}
			return
		}
		renderResult(w, true, "")
		ch <- callback{code: code}
	})

	listener, err := net.Listen("tcp", "127.0.0.1:"+listenPort)
	if err != nil {
		log.Fatalf("bluerelief-auth: listen :%s: %v (already running?)", listenPort, err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			ch <- callback{err: err}
		}
	}()

	fmt.Println("Opening Spotify authorization page in your browser…")
	fmt.Println("If it does not open automatically, visit:")
	fmt.Println(" ", authURL)
	_ = openBrowser(authURL)

	// Bounded wait — if the user closes the browser, don't hang forever.
	var cb callback
	select {
	case cb = <-ch:
	case <-time.After(5 * time.Minute):
		log.Fatal("bluerelief-auth: timed out waiting for authorization (5m)")
	}
	_ = srv.Shutdown(context.Background())

	if cb.err != nil {
		log.Fatalf("bluerelief-auth: %v", cb.err)
	}

	tok, err := exchange(context.Background(), clientID, redirect, cb.code, verifier)
	if err != nil {
		log.Fatalf("bluerelief-auth: exchange: %v", err)
	}

	if err := writeToken(outPath, tok); err != nil {
		log.Fatalf("bluerelief-auth: write %s: %v", outPath, err)
	}

	fmt.Printf("\n✓ Saved token to %s\n", outPath)
	fmt.Println("Next step: copy it to the board, e.g.")
	fmt.Printf("  scp %s Rock:/tmp/ && \\\n", outPath)
	fmt.Println("  ssh Rock 'sudo install -m 0600 -o rock -g rock /tmp/spotify-token.json /var/lib/BlueRelief/spotify-token.json'")
}

// pkcePair returns a (verifier, challenge) pair per RFC 7636. The verifier is
// 43–128 unreserved chars; the challenge is the base64url-no-pad SHA-256 of
// the verifier. Spotify only accepts the S256 method.
func pkcePair() (string, string, error) {
	verifier, err := randomString(64)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// randomString returns n bytes of cryptographic randomness, base64url-encoded
// with no padding. Length of the output is ceil(n*4/3) chars, so n=64 yields
// 86 chars — well within the 43–128 PKCE range.
func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func buildAuthURL(clientID, redirect, challenge, state string) string {
	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", redirect)
	q.Set("code_challenge_method", "S256")
	q.Set("code_challenge", challenge)
	q.Set("state", state)
	q.Set("scope", strings.Join(spotify.Scopes, " "))
	return spotify.AuthorizeURL + "?" + q.Encode()
}

func exchange(ctx context.Context, clientID, redirect, code, verifier string) (spotify.Token, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirect)
	form.Set("client_id", clientID)
	form.Set("code_verifier", verifier)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, spotify.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return spotify.Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return spotify.Token{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return spotify.Token{}, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var payload struct {
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		Scope        string `json:"scope"`
		ExpiresIn    int    `json:"expires_in"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return spotify.Token{}, fmt.Errorf("decode: %w", err)
	}
	if payload.RefreshToken == "" {
		return spotify.Token{}, errors.New("Spotify returned no refresh_token — the appliance needs it")
	}
	return spotify.Token{
		AccessToken:  payload.AccessToken,
		RefreshToken: payload.RefreshToken,
		TokenType:    payload.TokenType,
		Scope:        payload.Scope,
		ExpiresAt:    time.Now().Add(time.Duration(payload.ExpiresIn) * time.Second),
	}, nil
}

func writeToken(path string, tok spotify.Token) error {
	data, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func openBrowser(target string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", target).Start()
	case "linux":
		return exec.Command("xdg-open", target).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", target).Start()
	}
	return errors.New("unsupported platform; open the URL manually")
}

// renderResult shows a minimal page in the browser tab so the user knows the
// flow finished and they can close the tab.
func renderResult(w http.ResponseWriter, ok bool, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if ok {
		fmt.Fprint(w, `<!doctype html><meta charset=utf-8><title>BlueRelief</title>
<body style="font-family:-apple-system,Helvetica,Arial,sans-serif;padding:48px;max-width:520px">
<h1 style="margin:0 0 8px">Authorized.</h1>
<p>You can close this tab and return to the terminal.</p>`)
		return
	}
	fmt.Fprintf(w, `<!doctype html><meta charset=utf-8><title>BlueRelief</title>
<body style="font-family:-apple-system,Helvetica,Arial,sans-serif;padding:48px;max-width:520px">
<h1 style="margin:0 0 8px;color:#c33">Authorization failed.</h1>
<p>%s</p>`, detail)
}

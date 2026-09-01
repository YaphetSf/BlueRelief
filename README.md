# BlueRelief

Music appliance on a Radxa ROCK 4C+. Spotify Connect (raspotify) and AirPlay
(shairport-sync) both write `/run/BlueRelief/state.json`; `bluerelief-web`
serves it on `:8085` over SSE; `bluerelief-gio` renders it under sway on a
1920×1080 DisplayPort touch panel.

```
raspotify ─┐                        ┌─ bluerelief-gio  (panel, sway)
           ├─ state.json ─ bluerelief-web :8085 ─┤
shairport ─┘                        └─ browser     (ssh -L)
```

## Deploy

```sh
./scripts/push-pull-rock.sh "commit message"
```

Commits, pushes, SSHes into `BlueRelief`, rebuilds, and installs + restarts
only what changed. It will not restart a feature you have switched off.

## Feature switches

Every feature is one key in `/etc/BlueRelief/bluerelief.conf` **on the board**.

```sh
ssh BlueRelief bluerelief-config                    # what's on
ssh BlueRelief 'sudo bluerelief-config PANEL off'   # set + apply
```

| Key | Values | `off` means |
|---|---|---|
| `SPOTIFY` | `on` `off` | no Spotify Connect |
| `AIRPLAY` | `on` `off` | no AirPlay receiver |
| `PANEL` | `on` `off` | no screen — compositor, UI and blanking all skipped |
| `WATCHDOG` | `on` `off` | no librespot unwedging probe |
| `LYRICS` | `on` `off` | no LRCLIB lookup |
| `SCREEN_IDLE` | `5m` `off` | idle time before the panel blanks |
| `AIRPLAY_CONTROL` | `mpris` `playerctl` `none` | AirPlay transport backend |
| `SPOTIFY_CLIENT_ID` | app ID | empty = read-only UI |
| `SPOTIFY_DEVICE_NAME` | device name | must match `LIBRESPOT_NAME` |

Switches are systemd `ExecCondition=`, so an `off` feature is **skipped** at
boot — `inactive`, no process, no logs. That is correct, not broken.

Hand-editing works; follow with `sudo bluerelief-config apply`. The board's copy
is the only copy — a deploy creates it once from
`etc/BlueRelief/bluerelief.conf.example` and never overwrites it.

## Check

```sh
ssh BlueRelief bluerelief-status        # switches, services, state, display
ssh BlueRelief 'journalctl -u bluerelief-web -n 50'
ssh -L 8085:127.0.0.1:8085 BlueRelief   # then open http://localhost:8085
```

## Host tunables

Board-level files the deploy syncs alongside the units. They are not features,
have no switch, and a reflashed board comes back without them — which is the
state the board was in when it hung twice in silence.

| File | Fixes |
|---|---|
| `etc/tmpfiles.d/rk3399-clocks.conf` | pins DDR at 328 MHz and caps the A72 pair at 1008 MHz — RK3399 DDR scaling switches clocks from ATF and hangs the SoC with nothing in the log; 328 MHz is where the governor already sat for 99% of uptime, and runs 6 °C cooler than pinning at the 666 MHz ceiling |
| `etc/systemd/system.conf.d/10-watchdog.conf` | arms the dw_wdt, so a hang costs a 60 s reboot instead of lasting until someone notices |
| `etc/systemd/journald.conf.d/10-sync.conf` | 10 s journal fsync — the 5 m default drops exactly the window that would explain a hang |
| `etc/systemd/system/wifi-regdomain.service` | `iw reg set GB` — cfg80211 is built into this kernel, so `modprobe.d` never reaches it and the board boots as regdomain 00 |
| `etc/NetworkManager/conf.d/20-wifi-powersave.conf` | Broadcom SDIO power save off — it cost 40 ms of median latency and read as a flaky link |

`bluerelief-status` reports the first four under **hang guards**; if the board
hangs again, that is the first thing to read.

## Preview on the Mac

```sh
go build ./cmd/bluerelief-web ./cmd/bluerelief-gio
./bluerelief-web --mock --mock-path fixtures/state-with-lyrics.json --addr :8085
./bluerelief-gio        # or just open http://localhost:8085
```

Fixtures: `state-playing`, `state-paused`, `state-disconnected`,
`state-airplay`, `state-with-lyrics`.

## One-time setup

**Spotify control link** — mint a token on the Mac, ship it, point the config
at your app:

```sh
export SPOTIFY_CLIENT_ID=...            # developer.spotify.com, PKCE,
go run ./cmd/bluerelief-auth            # redirect http://127.0.0.1:8888/callback
scp spotify-token.json BlueRelief:/tmp/
ssh BlueRelief 'sudo install -D -m 0600 -o ding -g ding \
  /tmp/spotify-token.json /var/lib/BlueRelief/spotify-token.json'
ssh BlueRelief "sudo bluerelief-config SPOTIFY_CLIENT_ID $SPOTIFY_CLIENT_ID"
```

Without it the UI is read-only: now-playing renders, transport and Browse are
disabled.

**AirPlay** — additive, runs alongside raspotify:

```sh
ssh BlueRelief
sudo apt install shairport-sync avahi-daemon dbus
sudo cp ~/BlueRelief/etc/shairport-sync/bluerelief-airplay.conf.example \
  /etc/shairport-sync.conf     # check output_device = "plughw:AUDIO,0"
sudo systemctl enable --now shairport-sync bluerelief-airplay-bridge
```

**Event bridge** — `scripts/install-event-bridge.sh`, only on a freshly flashed
board. It rewrites `/etc/raspotify/conf` and bounces raspotify.

## Layout

```
cmd/          bluerelief-web · -gio · -screen · -auth
internal/     state schema · spotify client · lyrics · embedded web assets
bin/          bluerelief-config · -status · -watchdog · spotify-event · airplay-event
etc/          systemd units · sway config · bluerelief.conf.example
scripts/      push-pull-rock.sh · install-event-bridge.sh
fixtures/     sample state.json for --mock
```

The UI is `internal/web/assets/` embedded via `go:embed`, so `go build` is the
frontend build.

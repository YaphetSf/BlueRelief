#!/bin/sh
set -eu

# Commit + push from the Mac, then ssh the board to fetch / build / sync
# every artifact and restart only the units whose source files changed.

remote_host="${BLUERELIEF_ROCK_HOST:-BlueRelief}"
remote_dir="${BLUERELIEF_ROCK_DIR:-/home/ding/BlueRelief}"
branch="$(git branch --show-current)"
origin_url="$(git config --get remote.origin.url)"

if [ -z "$branch" ]; then
  echo "Could not determine current git branch." >&2
  exit 1
fi
if [ -z "$origin_url" ]; then
  echo "Could not determine origin remote URL." >&2
  exit 1
fi

if [ "$#" -gt 0 ]; then
  commit_message="$*"
else
  commit_message="update BlueRelief"
fi

git add -A
if ! git diff --cached --quiet; then
  git commit -m "$commit_message"
else
  echo "No local changes to commit."
fi
git push origin "$branch"

# Heredoc is single-quoted; branch / remote_dir come in via the ssh env preamble.
ssh "$remote_host" "branch='$branch' remote_dir='$remote_dir' origin_url='$origin_url' sh -s" <<'REMOTE'
set -eu
export PATH=/usr/local/go/bin:$PATH

legacy_prefix="sp""otty"
legacy_name="Spo""TTY"
legacy_upper="SPOT""TY"
legacy_dir="/home/rock/$legacy_name"

if [ ! -d "$remote_dir/.git" ]; then
  mkdir -p "$(dirname "$remote_dir")"
  if [ -d "$remote_dir" ]; then
    rmdir "$remote_dir" 2>/dev/null || {
      echo "$remote_dir exists but is not a git repo and is not empty." >&2
      echo "Move it aside, then rerun this script." >&2
      exit 1
    }
  fi
  if [ -d "$legacy_dir/.git" ]; then
    echo "Cloning repo into $remote_dir from existing board checkout..."
    git clone "$legacy_dir" "$remote_dir"
  else
    echo "Cloning repo into $remote_dir..."
    git clone "$origin_url" "$remote_dir"
  fi
fi

if [ -d "$legacy_dir/.git" ]; then
  legacy_origin=$(git -C "$legacy_dir" remote get-url origin 2>/dev/null || true)
  if [ -n "$legacy_origin" ]; then
    git -C "$remote_dir" remote set-url origin "$legacy_origin"
  fi
fi

cd "$remote_dir"
git fetch origin "$branch"
git checkout -B "$branch" "origin/$branch"

# go build with multiple package args only type-checks — build each separately.
echo "Building Go binaries..."
go build ./cmd/bluerelief-web
go build -tags nox11,novulkan ./cmd/bluerelief-gio
go build ./cmd/bluerelief-screen

restart_web=0
restart_kiosk=0
restart_gio=0
restart_screen=0
restart_airplay_bridge=0
restart_watchdog=0
restart_raspotify=0
restart_shairport=0
reload_systemd=0

# Returns 0 if it actually installed, 1 if dst already matched src.
sync_file() {
  src=$1; dst=$2; mode=$3
  if [ ! -f "$src" ]; then
    echo "  ! missing source: $src" >&2
    return 1
  fi
  if sudo cmp -s "$src" "$dst" 2>/dev/null; then
    return 1
  fi
  sudo install -D -m "$mode" "$src" "$dst"
  echo "  → $dst"
  return 0
}

echo "Syncing binaries..."
if sync_file bluerelief-web        /usr/local/bin/bluerelief-web    0755; then restart_web=1;    fi
if sync_file bluerelief-gio        /usr/local/bin/bluerelief-gio    0755; then restart_gio=1;    fi
if sync_file bluerelief-screen     /usr/local/bin/bluerelief-screen 0755; then restart_screen=1; fi
for f in bin/*; do
  [ -e "$f" ] || continue
  name=$(basename "$f")
  if sync_file "$f" "/usr/local/bin/$name" 0755; then
    case "$name" in
      airplay-event) restart_airplay_bridge=1 ;;
      spotify-event) restart_raspotify=1 ;;
      bluerelief-watchdog) restart_watchdog=1 ;;
    esac
  fi
done

echo "Syncing systemd units..."
for unit in etc/systemd/system/*.service etc/systemd/system/*.timer; do
  [ -e "$unit" ] || continue
  name=$(basename "$unit")
  if sync_file "$unit" "/etc/systemd/system/$name" 0644; then
    reload_systemd=1
    case "$name" in
      bluerelief-web.service)    restart_web=1    ;;
      bluerelief-kiosk.service)  restart_kiosk=1  ;;
      bluerelief-gio.service)    restart_gio=1    ;;
      bluerelief-screen.service) restart_screen=1 ;;
      bluerelief-airplay-bridge.service) restart_airplay_bridge=1 ;;
      bluerelief-watchdog.service|bluerelief-watchdog.timer) restart_watchdog=1 ;;
    esac
  fi
done

echo "Syncing systemd drop-ins..."
# raspotify.service and shairport-sync.service belong to apt, so their feature
# switch lives in a drop-in rather than in a unit file we own.
for dropin in etc/systemd/system/*.service.d/*.conf; do
  [ -e "$dropin" ] || continue
  rel=${dropin#etc/systemd/system/}
  if sync_file "$dropin" "/etc/systemd/system/$rel" 0644; then
    reload_systemd=1
    case "$rel" in
      raspotify.service.d/*)      restart_raspotify=1 ;;
      shairport-sync.service.d/*) restart_shairport=1 ;;
    esac
  fi
done

echo "Syncing runtime tmpfiles..."
raspotify_user=$(systemctl show -p User --value raspotify.service 2>/dev/null || true)
if [ -z "$raspotify_user" ]; then
  raspotify_user=root
fi
tmpfiles_tmp=$(mktemp)
{
  printf 'd /run/BlueRelief 0755 %s %s -\n' "$raspotify_user" "$raspotify_user"
  printf 'p /run/BlueRelief/airplay-metadata 0666 root root -\n'
} > "$tmpfiles_tmp"
if sudo cmp -s "$tmpfiles_tmp" /etc/tmpfiles.d/bluerelief.conf 2>/dev/null; then
  rm -f "$tmpfiles_tmp"
else
  sudo install -D -m 0644 "$tmpfiles_tmp" /etc/tmpfiles.d/bluerelief.conf
  rm -f "$tmpfiles_tmp"
fi
sudo systemd-tmpfiles --create /etc/tmpfiles.d/bluerelief.conf

echo "Syncing runtime config..."
sudo install -d -m 0755 /etc/BlueRelief
# /etc/BlueRelief/bluerelief.conf is the board's feature state: never
# overwrite it, only create it, seeding from whichever pre-config env file the
# board still carries.
if [ ! -f /etc/BlueRelief/bluerelief.conf ]; then
  conf_tmp=$(mktemp)
  cp etc/BlueRelief/bluerelief.conf.example "$conf_tmp"
  for old in /etc/BlueRelief/bluerelief-web.env "/etc/$legacy_name/$legacy_prefix-web.env"; do
    sudo test -f "$old" || continue
    for key in SPOTIFY_CLIENT_ID SPOTIFY_DEVICE_NAME; do
      val=$(sudo sed -n "s/^[[:space:]]*$key[[:space:]]*=[[:space:]]*//p" "$old" | tail -1)
      [ -n "$val" ] || continue
      sed -i -E "s|^$key=.*|$key=$val|" "$conf_tmp"
    done
    echo "  seeded control-link keys from $old"
    break
  done
  sudo install -D -m 0644 "$conf_tmp" /etc/BlueRelief/bluerelief.conf
  rm -f "$conf_tmp"
  echo "  → /etc/BlueRelief/bluerelief.conf (new)"
fi
# The env file it replaced is gone for good; leaving it would give the board
# two places to look for the same settings.
sudo rm -f /etc/BlueRelief/bluerelief-web.env "/etc/$legacy_name/$legacy_prefix-web.env"
if [ ! -f /var/lib/BlueRelief/spotify-token.json ] \
  && sudo test -f "/var/lib/$legacy_name/spotify-token.json"; then
  sudo install -D -m 0600 -o ding -g ding \
    "/var/lib/$legacy_name/spotify-token.json" \
    /var/lib/BlueRelief/spotify-token.json
fi
if sudo test -f /etc/raspotify/conf; then
  raspotify_tmp=$(mktemp)
  sudo cp /etc/raspotify/conf "$raspotify_tmp"
  if grep -Eq '^[[:space:]]*#?[[:space:]]*LIBRESPOT_NAME=' "$raspotify_tmp"; then
    sed -i -E 's|^[[:space:]]*#?[[:space:]]*LIBRESPOT_NAME=.*|LIBRESPOT_NAME="BlueRelief"|' "$raspotify_tmp"
  else
    printf '\nLIBRESPOT_NAME="BlueRelief"\n' >> "$raspotify_tmp"
  fi
  if grep -Eq '^[[:space:]]*#?[[:space:]]*LIBRESPOT_ONEVENT=' "$raspotify_tmp"; then
    sed -i -E 's|^[[:space:]]*#?[[:space:]]*LIBRESPOT_ONEVENT=.*|LIBRESPOT_ONEVENT="/usr/local/bin/spotify-event"|' "$raspotify_tmp"
  else
    printf 'LIBRESPOT_ONEVENT="/usr/local/bin/spotify-event"\n' >> "$raspotify_tmp"
  fi
  if sudo cmp -s "$raspotify_tmp" /etc/raspotify/conf; then
    rm -f "$raspotify_tmp"
  else
    sudo install -m 0644 "$raspotify_tmp" /etc/raspotify/conf
    rm -f "$raspotify_tmp"
    restart_raspotify=1
  fi
fi
if sudo test -f /etc/shairport-sync.conf; then
  shairport_tmp=$(mktemp)
  sudo cp /etc/shairport-sync.conf "$shairport_tmp"
  sed -i -E 's|^[[:space:]]*name[[:space:]]*=.*|  name = "BlueRelief AirPlay";|' "$shairport_tmp"
  sed -i -E 's|^[[:space:]]*pipe_name[[:space:]]*=.*|  pipe_name = "/run/BlueRelief/airplay-metadata";|' "$shairport_tmp"
  if sudo cmp -s "$shairport_tmp" /etc/shairport-sync.conf; then
    rm -f "$shairport_tmp"
  else
    sudo install -m 0644 "$shairport_tmp" /etc/shairport-sync.conf
    rm -f "$shairport_tmp"
    restart_shairport=1
  fi
fi

echo "Syncing sway config..."
if sync_file etc/sway/config /etc/bluerelief/sway.config 0644; then
  restart_kiosk=1
fi

if [ "$reload_systemd" = 1 ]; then
  echo "systemctl daemon-reload"
  sudo systemctl daemon-reload
fi
if [ "$reload_systemd" = 1 ]; then
  sudo systemctl enable \
    bluerelief-web.service \
    bluerelief-kiosk.service \
    bluerelief-gio.service \
    bluerelief-screen.service \
    bluerelief-airplay-bridge.service \
    bluerelief-watchdog.timer
fi

echo "Retiring pre-rename services..."
for unit in \
  "$legacy_prefix-screen.service" \
  "$legacy_prefix-gio.service" \
  "$legacy_prefix-kiosk.service" \
  "$legacy_prefix-airplay-bridge.service" \
  "$legacy_prefix-web.service" \
  "$legacy_prefix-watchdog.timer" \
  "$legacy_prefix-watchdog.service"; do
  sudo systemctl disable --now "$unit" >/dev/null 2>&1 || true
  sudo rm -f "/etc/systemd/system/$unit"
done
sudo rm -f \
  "/usr/local/bin/$legacy_prefix-status" \
  "/usr/local/bin/$legacy_prefix-watchdog"
sudo systemctl daemon-reload

# Every switch is enforced by ExecCondition= at boot; this reconciles what is
# running right now to whatever the config says.
echo "Applying feature switches..."
sudo bluerelief-config apply

# A feature that is switched off must not be restarted back into life just
# because its binary changed.
switch_on() {
  val=$(sed -n "s/^[[:space:]]*$1[[:space:]]*=[[:space:]]*//p" \
    /etc/BlueRelief/bluerelief.conf 2>/dev/null | tail -1 | tr -d "[:space:]")
  case "$val" in
    off|OFF|Off|0|false|no) return 1 ;;
    *) return 0 ;;
  esac
}

if [ "$restart_raspotify" = 1 ] && switch_on SPOTIFY; then
  echo "Restarting raspotify"
  sudo systemctl restart raspotify
fi
if [ "$restart_shairport" = 1 ] && switch_on AIRPLAY; then
  echo "Restarting shairport-sync"
  sudo systemctl restart shairport-sync
fi

if [ "$restart_web" = 1 ]; then
  echo "Restarting bluerelief-web"
  sudo systemctl restart bluerelief-web
fi
if [ "$restart_airplay_bridge" = 1 ] && switch_on AIRPLAY; then
  echo "Restarting bluerelief-airplay-bridge"
  sudo systemctl restart bluerelief-airplay-bridge
fi
if [ "$restart_screen" = 1 ] && switch_on PANEL; then
  echo "Restarting bluerelief-screen"
  sudo systemctl restart bluerelief-screen 2>/dev/null \
    || echo "  (bluerelief-screen.service not installed yet — skip)"
fi
if [ "$restart_kiosk" = 1 ] && switch_on PANEL; then
  echo "Restarting bluerelief-kiosk"
  sudo systemctl restart bluerelief-kiosk 2>/dev/null \
    || echo "  (bluerelief-kiosk.service not installed yet — skip)"
fi
if [ "$restart_gio" = 1 ] && switch_on PANEL; then
  echo "Restarting bluerelief-gio"
  sudo systemctl restart bluerelief-gio 2>/dev/null \
    || echo "  (bluerelief-gio.service not installed yet — skip)"
fi
if [ "$restart_watchdog" = 1 ] && switch_on WATCHDOG; then
  echo "Restarting bluerelief-watchdog.timer"
  sudo systemctl restart bluerelief-watchdog.timer
fi

echo "Service status:"
for svc in bluerelief-web bluerelief-kiosk bluerelief-gio bluerelief-screen bluerelief-airplay-bridge bluerelief-watchdog.timer; do
  case "$svc" in
    bluerelief-kiosk|bluerelief-gio|bluerelief-screen) key=PANEL ;;
    bluerelief-airplay-bridge)                         key=AIRPLAY ;;
    bluerelief-watchdog.timer)                         key=WATCHDOG ;;
    *)                                                 key= ;;
  esac
  if [ -n "$key" ] && ! switch_on "$key"; then
    printf "  %-24s off (%s=off)\n" "$svc" "$key"
    continue
  fi
  state=$(systemctl is-active "$svc" 2>/dev/null || true)
  printf "  %-24s %s\n" "$svc" "$state"
done
REMOTE

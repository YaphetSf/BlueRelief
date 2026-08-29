#!/bin/sh
set -eu

repo_root="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
raspotify_conf="/etc/raspotify/conf"
onevent_line='LIBRESPOT_ONEVENT="/usr/local/bin/spotify-event"'
tmpfiles_conf="/etc/tmpfiles.d/bluerelief.conf"

if [ "$(id -u)" -ne 0 ]; then
  echo "Run this installer with sudo on the ROCK4C Plus." >&2
  exit 1
fi

if ! systemctl cat raspotify.service >/dev/null 2>&1; then
  echo "Missing raspotify.service; install Raspotify before running this script." >&2
  exit 1
fi

raspotify_user="$(systemctl show -p User --value raspotify.service 2>/dev/null || true)"
if [ -z "$raspotify_user" ]; then
  raspotify_user="root"
fi

if ! id "$raspotify_user" >/dev/null 2>&1; then
  echo "raspotify.service declares User=$raspotify_user, but that user does not exist." >&2
  exit 1
fi

install -D -m 0755 "$repo_root/bin/spotify-event" /usr/local/bin/spotify-event
install -D -m 0755 "$repo_root/bin/airplay-event" /usr/local/bin/airplay-event
{
  printf 'd /run/BlueRelief 0755 %s %s -\n' "$raspotify_user" "$raspotify_user"
  printf 'p /run/BlueRelief/airplay-metadata 0666 root root -\n'
} > "$tmpfiles_conf"
systemd-tmpfiles --create "$tmpfiles_conf"

if [ ! -f "$raspotify_conf" ]; then
  echo "Missing $raspotify_conf; install Raspotify before running this script." >&2
  exit 1
fi

if grep -Eq '^[[:space:]]*#?[[:space:]]*LIBRESPOT_ONEVENT=' "$raspotify_conf"; then
  sed -i -E "s|^[[:space:]]*#?[[:space:]]*LIBRESPOT_ONEVENT=.*|$onevent_line|" "$raspotify_conf"
else
  printf '\n%s\n' "$onevent_line" >> "$raspotify_conf"
fi

systemctl restart raspotify.service
systemctl --no-pager --full status raspotify.service

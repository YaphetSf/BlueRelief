(() => {
  "use strict";

  const els = {
    body: document.body,
    app: document.getElementById("app"),
    cover: document.getElementById("cover"),
    status: document.getElementById("status"),
    authWarn: document.getElementById("auth-warn"),
    title: document.getElementById("title"),
    artist: document.getElementById("artist"),
    album: document.getElementById("album"),
    progressBar: document.getElementById("progress-bar"),
    fill: document.getElementById("progress-fill"),
    position: document.getElementById("position"),
    duration: document.getElementById("duration"),
    volReadout: document.getElementById("vol-readout"),
    btnPrev: document.getElementById("btn-prev"),
    btnPlay: document.getElementById("btn-play"),
    btnNext: document.getElementById("btn-next"),
    btnVolDn: document.getElementById("btn-vol-dn"),
    btnVolUp: document.getElementById("btn-vol-up"),
    lyricsPane: document.getElementById("lyrics-pane"),
    lyricsLines: document.getElementById("lyrics-lines"),
  };

  let state = null;
  let coverKey = "";
  let authStatus = { authorized: false, airplay_control: true };

  /* ── Optimistic overlay ──────────────────────────────────────────────
     A control press updates the UI *before* the server-side state
     changes. Without this, every tap looks dead for ~0.5–1.5s (Web API
     round-trip + librespot event + state.json poll + SSE).

     We store the optimistic values keyed by field, with an expiry. When a
     fresh SSE state arrives that already reflects the change, we drop
     the override. If the server never confirms (e.g. control failed),
     the override decays after `optimisticTTL` and the real state takes
     over — no permanent UI lies. */
  const optimisticTTL = 2500;
  let optimistic = {};
  function setOptimistic(field, value) {
    optimistic[field] = { value, until: Date.now() + optimisticTTL };
    render();
  }
  function readField(field, real) {
    const o = optimistic[field];
    if (o && Date.now() < o.until) return o.value;
    if (o) delete optimistic[field];
    return real;
  }

  function format(ms) {
    if (!Number.isFinite(ms) || ms < 0) ms = 0;
    const s = Math.floor(ms / 1000);
    const m = Math.floor(s / 60);
    return `${m}:${String(s % 60).padStart(2, "0")}`;
  }

  function statusLabel(raw) {
    if (!raw) return "Disconnected";
    const text = raw.replace(/_/g, " ");
    return text.charAt(0).toUpperCase() + text.slice(1);
  }

  function sourceKind(s = state) {
    return String(s?.source?.kind || "spotify").toLowerCase();
  }

  function sourceLabel(s = state) {
    const name = s?.source?.name;
    if (typeof name === "string" && name.trim()) return name.trim().toUpperCase();
    const kind = sourceKind(s);
    if (kind === "airplay") return "AIRPLAY";
    if (kind === "idle") return "IDLE";
    return "SPOTIFY";
  }

  function effectiveCapabilities(s = state) {
    const caps = s?.capabilities || {};
    const hasCaps = ["transport", "volume", "seek", "browse", "queue"].some(
      (key) => caps[key] === true
    );
    if (!hasCaps && !s?.source?.kind) {
      return { transport: true, volume: true, seek: true, browse: true, queue: false };
    }
    return {
      transport: !!caps.transport,
      volume: !!caps.volume,
      seek: !!caps.seek,
      browse: !!caps.browse,
      queue: !!caps.queue,
    };
  }

  function can(capability) {
    if (!state) return false;
    const kind = sourceKind();
    if (kind === "idle") return false;
    if (!effectiveCapabilities()[capability]) return false;
    if (kind === "spotify" && !authStatus.authorized) return false;
    if (kind === "airplay" && authStatus.airplay_control === false) return false;
    return true;
  }

  function controlPath(path) {
    const sep = path.includes("?") ? "&" : "?";
    return `${path}${sep}source=${encodeURIComponent(sourceKind())}`;
  }

  function updateControls() {
    const transport = can("transport");
    const volume = can("volume");
    const seek = can("seek");
    for (const b of [els.btnPrev, els.btnPlay, els.btnNext]) {
      b.disabled = !transport;
    }
    for (const b of [els.btnVolDn, els.btnVolUp]) {
      b.disabled = !volume;
    }
    els.progressBar.dataset.enabled = String(seek);
    els.progressBar.setAttribute("aria-disabled", String(!seek));

    const needsSpotifyAuth = sourceKind() === "spotify" && !authStatus.authorized;
    els.body.dataset.auth = String(!needsSpotifyAuth);
    els.authWarn.hidden = !needsSpotifyAuth;
  }

  function pickCover(track) {
    if (!track || !Array.isArray(track.covers)) return "";
    for (const url of track.covers) {
      if (typeof url === "string" && url.trim()) return url.trim();
    }
    return "";
  }

  function estimatedPosition(s) {
    const base = s?.playback?.position_ms ?? 0;
    if (!s?.playback?.is_playing || !s?.playback?.position_updated_at) return base;
    const updated = Date.parse(s.playback.position_updated_at);
    if (!Number.isFinite(updated)) return base;
    const delta = Date.now() - updated;
    if (delta <= 0) return base;
    const duration = s?.track?.duration_ms ?? 0;
    const next = base + delta;
    return duration > 0 ? Math.min(next, duration) : next;
  }

  function render() {
    if (!state) return;
    const track = state.track;
    const playback = state.playback || {};
    const settings = state.settings || {};

    const isPlaying = readField("is_playing", !!playback.is_playing);
    const rawStatus = playback.status || "";
    els.body.dataset.source = sourceKind();
    const statusKey = isPlaying
      ? "playing"
      : rawStatus === "disconnected" || !rawStatus
      ? "disconnected"
      : "paused";
    els.body.dataset.state = statusKey;
    els.status.textContent = `${sourceLabel()} · ${statusLabel(playback.status)}`;
    // play/pause icon swap is driven by body[data-state] in CSS

    els.title.textContent = track?.name || "—";
    els.artist.textContent =
      (track?.artists?.length ? track.artists : track?.album_artists || []).join(", ") || "—";
    els.album.textContent = track?.album || "—";

    const nextCover = pickCover(track);
    if (nextCover !== coverKey) {
      coverKey = nextCover;
      els.cover.innerHTML = "";
      if (nextCover) {
        const img = document.createElement("img");
        img.src = nextCover;
        img.alt = "";
        img.onerror = () => {
          els.cover.innerHTML = '<div class="cover-placeholder">BlueRelief</div>';
        };
        els.cover.appendChild(img);
      } else {
        els.cover.innerHTML = '<div class="cover-placeholder">BlueRelief</div>';
      }
      els.body.style.setProperty(
        "--ambient-url",
        nextCover ? `url("${nextCover}")` : "none"
      );
    }

    const duration = track?.duration_ms ?? 0;
    const position = estimatedPosition(state);
    els.position.textContent = format(position);
    els.duration.textContent = duration ? format(duration) : "—:—";
    els.fill.style.width = duration ? `${Math.min(100, (position / duration) * 100)}%` : "0%";

    const vol = readField("volume_percent", settings.volume_percent);
    els.volReadout.textContent = Number.isFinite(vol) ? `${vol}%` : "—";

    const lyrics = state.lyrics;
    if (lyrics && Array.isArray(lyrics.lines) && lyrics.lines.length > 0) {
      els.app.dataset.lyrics = "true";
      els.lyricsPane.hidden = false;
      renderLyrics(lyrics, position);
    } else {
      els.app.dataset.lyrics = "false";
      els.lyricsPane.hidden = true;
      els.lyricsLines.innerHTML = "";
      renderedLyricsKey = "";
    }

    updateControls();
  }

  let renderedLyricsKey = "";
  function renderLyrics(lyrics, position) {
    const key = state.track?.id || state.track?.uri || "";
    if (key !== renderedLyricsKey) {
      renderedLyricsKey = key;
      els.lyricsLines.innerHTML = "";
      for (const line of lyrics.lines) {
        const li = document.createElement("li");
        li.textContent = line.text || "";
        li.dataset.time = line.time_ms ?? 0;
        els.lyricsLines.appendChild(li);
      }
    }
    let activeIndex = -1;
    const items = els.lyricsLines.children;
    for (let i = 0; i < items.length; i++) {
      const t = Number(items[i].dataset.time);
      if (t <= position) activeIndex = i;
      else break;
    }
    for (let i = 0; i < items.length; i++) {
      items[i].classList.toggle("active", i === activeIndex);
    }
    if (activeIndex >= 0) {
      const item = items[activeIndex];
      const offset =
        item.offsetTop + item.offsetHeight / 2 - els.lyricsPane.clientHeight / 2;
      els.lyricsLines.style.transform = `translateY(${-offset}px)`;
    }
  }

  function tick() {
    if (state) render();
  }
  setInterval(tick, 250);

  /* ── SSE / state stream ────────────────────────────────────────────── */

  function connect() {
    const source = new EventSource("/api/events");
    source.onmessage = (evt) => {
      try {
        state = JSON.parse(evt.data);
        render();
      } catch (err) {
        console.error("bad event payload", err);
      }
    };
    source.onerror = () => {
      source.close();
      setTimeout(connect, 1500);
    };
  }

  fetch("/api/state")
    .then((r) => (r.ok ? r.json() : null))
    .then((s) => {
      if (s) {
        state = s;
        render();
      }
    })
    .catch(() => {})
    .finally(connect);

  /* ── Control link ───────────────────────────────────────────────────
     Each POST returns 204 (No Content). Failures are toast-less for now —
     the optimistic overlay decays after `optimisticTTL`, so the UI
     self-corrects if the server rejected the command. */

  async function post(path, body) {
    try {
      const res = await fetch(path, {
        method: "POST",
        headers: body ? { "Content-Type": "application/json" } : undefined,
        body: body ? JSON.stringify(body) : undefined,
      });
      if (!res.ok) {
        const text = await res.text();
        console.warn(path, res.status, text);
      }
      return res.ok;
    } catch (err) {
      console.warn(path, err);
      return false;
    }
  }

  function clamp(n, lo, hi) {
    return Math.max(lo, Math.min(hi, n));
  }

  els.btnPlay.addEventListener("click", () => {
    if (!can("transport")) return;
    const next = !state?.playback?.is_playing;
    setOptimistic("is_playing", next);
    post(controlPath(next ? "/api/control/play" : "/api/control/pause"));
  });
  els.btnPrev.addEventListener("click", () => {
    if (can("transport")) post(controlPath("/api/control/previous"));
  });
  els.btnNext.addEventListener("click", () => {
    if (can("transport")) post(controlPath("/api/control/next"));
  });

  els.btnVolDn.addEventListener("click", () => {
    if (!can("volume")) return;
    const cur = readField("volume_percent", state?.settings?.volume_percent ?? 50);
    const next = clamp((cur ?? 50) - 5, 0, 100);
    setOptimistic("volume_percent", next);
    post(controlPath(`/api/control/volume?percent=${next}`));
  });
  els.btnVolUp.addEventListener("click", () => {
    if (!can("volume")) return;
    const cur = readField("volume_percent", state?.settings?.volume_percent ?? 50);
    const next = clamp((cur ?? 50) + 5, 0, 100);
    setOptimistic("volume_percent", next);
    post(controlPath(`/api/control/volume?percent=${next}`));
  });

  /* Seek on tap. We don't drag — touch sliders on this panel are jittery
     and a single tap-to-position is precise enough. */
  els.progressBar.addEventListener("click", (evt) => {
    if (!can("seek")) return;
    const duration = state?.track?.duration_ms || 0;
    if (!duration) return;
    const rect = els.progressBar.getBoundingClientRect();
    const ratio = clamp((evt.clientX - rect.left) / rect.width, 0, 1);
    const ms = Math.round(ratio * duration);
    // Optimistic by mutating position locally (not via setOptimistic, since
    // that field isn't part of the readField scheme — just paint the bar).
    if (state?.playback) {
      state.playback.position_ms = ms;
      state.playback.position_updated_at = new Date().toISOString();
      render();
    }
    post(controlPath(`/api/control/seek?ms=${ms}`));
  });

  /* ── Auth gate ─────────────────────────────────────────────────────── */
  fetch("/api/auth/status")
    .then((r) => r.json())
    .then((s) => {
      authStatus = {
        authorized: !!s?.authorized,
        airplay_control: s?.airplay_control !== false,
      };
      updateControls();
    })
    .catch(() => {});
})();

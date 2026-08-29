package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadTrackState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	body := `{
		"schema_version": 1,
		"updated_at": "2026-05-14T13:38:52.989Z",
		"session": {"connected": true},
		"playback": {"status": "playing", "is_playing": true, "position_ms": 1234},
		"settings": {"volume_percent": 70, "shuffle": true, "repeat": "off"},
		"track": {
			"id": "track-1",
			"name": "Dani California",
			"artists": ["Red Hot Chili Peppers"],
			"album": "Stadium Arcadium",
			"duration_ms": 282160,
			"covers": ["", "https://example.invalid/cover.jpg"]
		}
	}`

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	snapshot, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if got := snapshot.ArtistText(); got != "Red Hot Chili Peppers" {
		t.Fatalf("ArtistText() = %q", got)
	}
	if got := snapshot.CoverKey(); got != "track-1" {
		t.Fatalf("CoverKey() = %q", got)
	}
	if got := snapshot.CoverURLs(); len(got) != 1 || got[0] != "https://example.invalid/cover.jpg" {
		t.Fatalf("CoverURLs() = %#v", got)
	}
	if filled, label := snapshot.Progress(10); filled != 0 || label != "0:01 / 4:42" {
		t.Fatalf("Progress() = %d, %q", filled, label)
	}
}

func TestLoadMissingFileReturnsDisconnectedState(t *testing.T) {
	snapshot, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := snapshot.StatusText(); got != "disconnected" {
		t.Fatalf("StatusText() = %q", got)
	}
}

func TestLoadBooleanRepeatState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	body := `{
		"playback": {"status": "playing"},
		"settings": {"repeat": false}
	}`

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	snapshot, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Settings.Repeat.String(); got != "off" {
		t.Fatalf("Repeat.String() = %q", got)
	}
}

func TestSourceAndCapabilitiesDefaultToSpotifyForLegacyState(t *testing.T) {
	snapshot := State{}
	if got := snapshot.SourceKind(); got != "spotify" {
		t.Fatalf("SourceKind() = %q", got)
	}
	if got := snapshot.SourceLabel(); got != "SPOTIFY" {
		t.Fatalf("SourceLabel() = %q", got)
	}
	if !snapshot.Can("transport") || !snapshot.Can("seek") || !snapshot.Can("browse") {
		t.Fatalf("legacy capabilities = %#v, want spotify controls enabled", snapshot.EffectiveCapabilities())
	}
}

func TestAirPlayCapabilitiesDisableSeekAndBrowse(t *testing.T) {
	snapshot := State{
		Source: Source{Kind: "airplay", Name: "AirPlay", Active: true},
		Capabilities: Capabilities{
			Transport: true,
			Volume:    true,
		},
	}
	if got := snapshot.SourceLabel(); got != "AIRPLAY" {
		t.Fatalf("SourceLabel() = %q", got)
	}
	if !snapshot.Can("transport") || !snapshot.Can("volume") {
		t.Fatalf("airplay transport/volume should be enabled")
	}
	if snapshot.Can("seek") || snapshot.Can("browse") {
		t.Fatalf("airplay seek/browse should be disabled")
	}
}

func TestProgressAtEstimatesPlayingPosition(t *testing.T) {
	position := 10_000
	duration := 60_000
	snapshot := State{
		Playback: Playback{
			Status:            "playing",
			IsPlaying:         true,
			PositionMS:        &position,
			PositionUpdatedAt: "2026-05-14T15:22:15.144Z",
		},
		Track: &Track{DurationMS: &duration},
	}
	now, err := time.Parse(time.RFC3339Nano, "2026-05-14T15:22:20.144Z")
	if err != nil {
		t.Fatal(err)
	}

	filled, label := snapshot.ProgressAt(12, now)
	if filled != 3 {
		t.Fatalf("filled = %d", filled)
	}
	if label != "0:15 / 1:00" {
		t.Fatalf("label = %q", label)
	}
}

func TestProgressAtDoesNotEstimatePausedPosition(t *testing.T) {
	position := 10_000
	duration := 60_000
	snapshot := State{
		Playback: Playback{
			Status:            "paused",
			IsPlaying:         false,
			PositionMS:        &position,
			PositionUpdatedAt: "2026-05-14T15:22:15.144Z",
		},
		Track: &Track{DurationMS: &duration},
	}
	now, err := time.Parse(time.RFC3339Nano, "2026-05-14T15:22:20.144Z")
	if err != nil {
		t.Fatal(err)
	}

	filled, label := snapshot.ProgressAt(12, now)
	if filled != 2 {
		t.Fatalf("filled = %d", filled)
	}
	if label != "0:10 / 1:00" {
		t.Fatalf("label = %q", label)
	}
}

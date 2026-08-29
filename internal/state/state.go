package state

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"time"
)

type State struct {
	SchemaVersion int          `json:"schema_version"`
	UpdatedAt     string       `json:"updated_at"`
	LastEvent     LastEvent    `json:"last_event"`
	Source        Source       `json:"source"`
	Capabilities  Capabilities `json:"capabilities"`
	Session       Session      `json:"session"`
	Playback      Playback     `json:"playback"`
	Track         *Track       `json:"track"`
	Settings      Settings     `json:"settings"`
}

type Source struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Active    bool   `json:"active"`
	UpdatedAt string `json:"updated_at"`
}

type Capabilities struct {
	Transport bool `json:"transport"`
	Volume    bool `json:"volume"`
	Seek      bool `json:"seek"`
	Browse    bool `json:"browse"`
	Queue     bool `json:"queue"`
}

type LastEvent struct {
	Name       string            `json:"name"`
	ReceivedAt string            `json:"received_at"`
	Fields     map[string]string `json:"fields"`
}

type Session struct {
	Connected      bool   `json:"connected"`
	UserName       string `json:"user_name"`
	ConnectionID   string `json:"connection_id"`
	ConnectedAt    string `json:"connected_at"`
	DisconnectedAt string `json:"disconnected_at"`
}

type Playback struct {
	Status            string `json:"status"`
	IsPlaying         bool   `json:"is_playing"`
	TrackID           string `json:"track_id"`
	PositionMS        *int   `json:"position_ms"`
	PositionUpdatedAt string `json:"position_updated_at"`
}

type Track struct {
	ItemType     string   `json:"item_type"`
	ID           string   `json:"id"`
	URI          string   `json:"uri"`
	Name         string   `json:"name"`
	DurationMS   *int     `json:"duration_ms"`
	IsExplicit   *bool    `json:"is_explicit"`
	Language     []string `json:"language"`
	Covers       []string `json:"covers"`
	Number       *int     `json:"number"`
	DiscNumber   *int     `json:"disc_number"`
	Popularity   *int     `json:"popularity"`
	Album        string   `json:"album"`
	Artists      []string `json:"artists"`
	AlbumArtists []string `json:"album_artists"`
	ShowName     string   `json:"show_name"`
	PublishTime  *int     `json:"publish_time"`
	Description  string   `json:"description"`
}

type Settings struct {
	VolumeRaw             *int   `json:"volume_raw"`
	VolumePercent         *int   `json:"volume_percent"`
	Shuffle               *bool  `json:"shuffle"`
	Repeat                Repeat `json:"repeat"`
	AutoPlay              *bool  `json:"auto_play"`
	FilterExplicitContent *bool  `json:"filter_explicit_content"`
}

type Repeat string

func (r *Repeat) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*r = ""
		return nil
	}

	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		*r = Repeat(text)
		return nil
	}

	var enabled bool
	if err := json.Unmarshal(data, &enabled); err == nil {
		if enabled {
			*r = "on"
		} else {
			*r = "off"
		}
		return nil
	}

	return fmt.Errorf("repeat must be bool, string, or null")
}

func (r Repeat) String() string {
	return string(r)
}

func Load(path string) (State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return State{Playback: Playback{Status: "disconnected"}}, err
	}

	var snapshot State
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return State{Playback: Playback{Status: "disconnected"}}, fmt.Errorf("read state: %w", err)
	}

	if snapshot.Playback.Status == "" {
		if snapshot.Session.Connected {
			snapshot.Playback.Status = "connected"
		} else {
			snapshot.Playback.Status = "disconnected"
		}
	}

	return snapshot, nil
}

func (s State) SourceKind() string {
	kind := strings.ToLower(strings.TrimSpace(s.Source.Kind))
	if kind == "" {
		// Backward compatibility for state.json files written before the
		// source contract existed: BlueRelief only had Spotify then.
		return "spotify"
	}
	return kind
}

func (s State) SourceLabel() string {
	if strings.TrimSpace(s.Source.Name) != "" {
		return strings.ToUpper(strings.TrimSpace(s.Source.Name))
	}
	switch s.SourceKind() {
	case "airplay":
		return "AIRPLAY"
	case "idle":
		return "IDLE"
	default:
		return "SPOTIFY"
	}
}

func (c Capabilities) IsZero() bool {
	return !c.Transport && !c.Volume && !c.Seek && !c.Browse && !c.Queue
}

func (s State) EffectiveCapabilities() Capabilities {
	caps := s.Capabilities
	if !caps.IsZero() {
		return caps
	}
	if s.SourceKind() == "spotify" {
		return Capabilities{
			Transport: true,
			Volume:    true,
			Seek:      true,
			Browse:    true,
		}
	}
	return caps
}

func (s State) Can(capability string) bool {
	caps := s.EffectiveCapabilities()
	switch strings.ToLower(strings.TrimSpace(capability)) {
	case "transport":
		return caps.Transport
	case "volume":
		return caps.Volume
	case "seek":
		return caps.Seek
	case "browse":
		return caps.Browse
	case "queue":
		return caps.Queue
	default:
		return false
	}
}

func (s State) StatusText() string {
	return s.Playback.StatusText()
}

func (p Playback) StatusText() string {
	if strings.TrimSpace(p.Status) == "" {
		return "disconnected"
	}
	return strings.ReplaceAll(p.Status, "_", " ")
}

func (s State) ArtistText() string {
	if s.Track == nil {
		return ""
	}
	if len(s.Track.Artists) > 0 {
		return strings.Join(s.Track.Artists, ", ")
	}
	return strings.Join(s.Track.AlbumArtists, ", ")
}

func (s State) CoverURLs() []string {
	if s.Track == nil {
		return nil
	}

	urls := make([]string, 0, len(s.Track.Covers))
	for _, url := range s.Track.Covers {
		url = strings.TrimSpace(url)
		if url != "" {
			urls = append(urls, url)
		}
	}
	return urls
}

func (s State) CoverKey() string {
	if s.Track == nil {
		return ""
	}
	if s.Track.ID != "" {
		return s.Track.ID
	}
	if s.Track.URI != "" {
		return s.Track.URI
	}
	urls := s.CoverURLs()
	if len(urls) > 0 {
		return urls[0]
	}
	return ""
}

func (s State) VolumeText() string {
	if s.Settings.VolumePercent == nil {
		return "-"
	}
	return fmt.Sprintf("%d%%", *s.Settings.VolumePercent)
}

func (s State) Progress(width int) (int, string) {
	return s.ProgressAt(width, time.Now())
}

func (s State) ProgressAt(width int, now time.Time) (int, string) {
	if width <= 0 {
		return 0, "-:-- / -:--"
	}

	position := s.EstimatedPositionMS(now)
	duration := 0
	if s.Track != nil {
		duration = intValue(s.Track.DurationMS)
	}

	if duration <= 0 {
		return 0, fmt.Sprintf("%s / -:--", formatMS(position))
	}

	position = clamp(position, 0, duration)
	filled := int(math.Round(float64(position) / float64(duration) * float64(width)))
	filled = clamp(filled, 0, width)

	return filled, fmt.Sprintf("%s / %s", formatMS(position), formatMS(duration))
}

func (s State) EstimatedPositionMS(now time.Time) int {
	position := intValue(s.Playback.PositionMS)
	if !s.Playback.IsPlaying || s.Playback.PositionUpdatedAt == "" {
		return position
	}

	updatedAt, err := time.Parse(time.RFC3339Nano, s.Playback.PositionUpdatedAt)
	if err != nil {
		return position
	}

	elapsed := now.Sub(updatedAt)
	if elapsed <= 0 {
		return position
	}

	position += int(elapsed / time.Millisecond)
	if s.Track != nil && s.Track.DurationMS != nil {
		position = clamp(position, 0, *s.Track.DurationMS)
	}
	return position
}

func intValue(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func clamp(value int, minValue int, maxValue int) int {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func formatMS(ms int) string {
	if ms < 0 {
		ms = 0
	}
	seconds := ms / 1000
	minutes := seconds / 60
	seconds %= 60
	return fmt.Sprintf("%d:%02d", minutes, seconds)
}

package main

import (
	"image"
	"image/color"
	"testing"

	"bluerelief/internal/state"
)

func TestExtractAlbumPalettePrefersSaturatedCoverColor(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 24, 24))
	for y := 0; y < 24; y++ {
		for x := 0; x < 24; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 218, G: 32, B: 54, A: 255})
		}
	}

	got := extractAlbumPalette(img)
	h, s, l := rgbToHSL(float64(got.Accent.R)/255, float64(got.Accent.G)/255, float64(got.Accent.B)/255)
	if h > 18 && h < 342 {
		t.Fatalf("accent hue = %.1f, want red hue near 0/360; color=%#v", h, got.Accent)
	}
	if s < 0.50 {
		t.Fatalf("accent saturation = %.2f, want saturated; color=%#v", s, got.Accent)
	}
	if l < 0.40 || l > 0.66 {
		t.Fatalf("accent lightness = %.2f, want UI-safe lightness; color=%#v", l, got.Accent)
	}
}

func TestMakeDiscMasksCornersAndKeepsCenter(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 40, 40))
	for y := 0; y < 40; y++ {
		for x := 0; x < 40; x++ {
			src.SetNRGBA(x, y, color.NRGBA{R: 10, G: 200, B: 90, A: 255})
		}
	}
	disc := makeDisc(src).(*image.NRGBA)
	if b := disc.Bounds(); b.Dx() != 40 || b.Dy() != 40 {
		t.Fatalf("disc size = %v, want 40x40", b.Size())
	}
	if _, _, _, a := disc.At(0, 0).RGBA(); a != 0 {
		t.Fatalf("corner alpha = %d, want fully transparent", a>>8)
	}
	r, g, bl, a := disc.At(20, 20).RGBA()
	if a>>8 != 255 || r>>8 != 10 || g>>8 != 200 || bl>>8 != 90 {
		t.Fatalf("center = (%d,%d,%d,%d), want opaque cover color", r>>8, g>>8, bl>>8, a>>8)
	}
}

func TestComposeBackgroundIsOpaque(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			src.SetNRGBA(x, y, color.NRGBA{R: 200, G: 40, B: 60, A: 255})
		}
	}
	bgImg := composeBackground(src, defaultPalette()).(*image.NRGBA)
	if b := bgImg.Bounds(); b.Dx() != 320 || b.Dy() != 180 {
		t.Fatalf("background size = %v, want 320x180", b.Size())
	}
	for _, p := range []image.Point{{0, 0}, {319, 179}, {160, 90}} {
		if _, _, _, a := bgImg.At(p.X, p.Y).RGBA(); a>>8 != 255 {
			t.Fatalf("background pixel %v alpha = %d, want opaque", p, a>>8)
		}
	}
}

func TestExtractAlbumPaletteFallsBackForNeutralCover(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 24, 24))
	for y := 0; y < 24; y++ {
		for x := 0; x < 24; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 120, G: 120, B: 120, A: 255})
		}
	}

	got := extractAlbumPalette(img)
	if got.Accent != defaultPalette().Accent {
		t.Fatalf("accent = %#v, want fallback %#v", got.Accent, defaultPalette().Accent)
	}
}

func TestFirstCoverAcceptsRelativeArtworkURL(t *testing.T) {
	s := &snapshot{}
	s.Track = &state.Track{Covers: []string{"/api/artwork/airplay?rev=abc"}}
	if got := firstCover(s); got != "/api/artwork/airplay?rev=abc" {
		t.Fatalf("firstCover = %q", got)
	}
}

func TestFirstCoverSkipsUnsupportedSchemes(t *testing.T) {
	s := &snapshot{}
	s.Track = &state.Track{Covers: []string{"file://tmp/cover.jpg", "https://example.com/cover.jpg"}}
	if got := firstCover(s); got != "https://example.com/cover.jpg" {
		t.Fatalf("firstCover = %q", got)
	}
}

func TestResolveCoverURLKeepsAbsoluteURL(t *testing.T) {
	u := &ui{baseURL: "http://127.0.0.1:8085"}
	got, err := u.resolveCoverURL("https://example.com/cover.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://example.com/cover.jpg" {
		t.Fatalf("resolved URL = %q", got)
	}
}

func TestResolveCoverURLUsesBaseURLForRelativeArtwork(t *testing.T) {
	u := &ui{baseURL: "http://127.0.0.1:8085"}
	got, err := u.resolveCoverURL("/api/artwork/airplay?rev=abc")
	if err != nil {
		t.Fatal(err)
	}
	want := "http://127.0.0.1:8085/api/artwork/airplay?rev=abc"
	if got != want {
		t.Fatalf("resolved URL = %q, want %q", got, want)
	}
}

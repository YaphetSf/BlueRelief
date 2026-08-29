// Command bluerelief-gio is a native Gio renderer for the BlueRelief kiosk.
//
// It intentionally keeps bluerelief-web as the backend: state still arrives over
// /api/events and controls still POST to /api/control/*. That makes the
// Chromium replacement a renderer swap, while preserving the remote web
// preview and the bluerelief-screen daemon.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"gioui.org/f32"
	"gioui.org/font"
	"image"
	"image/color"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"gioui.org/app"
	"gioui.org/font/gofont"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"bluerelief/internal/lyrics"
	"bluerelief/internal/state"
)

const (
	defaultBaseURL        = "http://127.0.0.1:8085"
	refreshPeriod         = 500 * time.Millisecond
	animFrameRate         = 33 * time.Millisecond
	vinylTurn             = 3600 * time.Millisecond
	lyricScrollTau        = 150 * time.Millisecond
	lyricFontSize         = unit.Sp(50)
	lyricBackHysteresisMS = 3000
)

var dbg bool
var forcePPDP float32

var (
	bg           = color.NRGBA{R: 5, G: 5, B: 7, A: 255}
	panel        = color.NRGBA{R: 28, G: 28, B: 34, A: 255}
	controlBg    = color.NRGBA{R: 48, G: 48, B: 56, A: 255}
	fg           = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
	muted        = color.NRGBA{R: 200, G: 200, B: 208, A: 255}
	lyricMuted   = color.NRGBA{R: 232, G: 232, B: 240, A: 255}
	accent       = color.NRGBA{R: 46, G: 224, B: 106, A: 255}
	paused       = color.NRGBA{R: 255, G: 207, B: 58, A: 255}
	disconnected = color.NRGBA{R: 154, G: 154, B: 162, A: 255}
)

type snapshot struct {
	state.State
	Lyrics *lyrics.Payload `json:"lyrics"`
}

type authStatus struct {
	Authorized     bool   `json:"authorized"`
	DeviceName     string `json:"device_name"`
	AirplayControl *bool  `json:"airplay_control"`
}

type update struct {
	state    *snapshot
	auth     *authStatus
	coverURL string
	cover    image.Image
	disc     image.Image
	bg       image.Image
	palette  *albumPalette
}

type albumPalette struct {
	Accent    color.NRGBA
	Secondary color.NRGBA
	Ink       color.NRGBA
}

type optimisticValue[T any] struct {
	value T
	until time.Time
}

type ui struct {
	baseURL string
	client  *http.Client
	sseClient *http.Client
	th      *material.Theme
	ops     op.Ops

	state *snapshot
	auth  authStatus

	coverURL string
	coverOp  paint.ImageOp
	hasCover bool
	discOp   paint.ImageOp
	hasDisc  bool
	bgOp     paint.ImageOp
	hasBg    bool
	palette  albumPalette

	play, prev, next, volDown, volUp, coverClick widget.Clickable
	power, powerYes, powerNo, powerScrim         widget.Clickable
	powerConfirm                                 bool
	seek                                         widget.Clickable
	lyricsList                                   layout.List
	vinylMode                                    bool
	vinylAngle                                   float64
	vinylLast                                    time.Time

	lyricScroll   float64
	lyricScrollAt time.Time
	lyricTarget   float64
	lyricTrackID  string
	lyricLine     int

	isPlaying *optimisticValue[bool]
	volume    *optimisticValue[int]

	updates chan update
	anim    atomic.Bool
	dbgLast time.Time
}

func main() {
	var baseURL, sizeFlag string
	flag.StringVar(&baseURL, "base-url", defaultBaseURL, "bluerelief-web base URL")
	flag.StringVar(&sizeFlag, "size", "1920x1080", "window size as WxH")
	flag.Parse()

	winW, winH := 1920, 1080
	fmt.Sscanf(sizeFlag, "%dx%d", &winW, &winH)
	dbg = os.Getenv("GIODBG") != ""
	fmt.Sscanf(os.Getenv("FORCE_PPDP"), "%f", &forcePPDP)

	baseURL = strings.TrimRight(baseURL, "/")
	u := &ui{
		baseURL: baseURL,
		client:  &http.Client{Timeout: 10 * time.Second},
		sseClient: &http.Client{},
		th:      material.NewTheme(),
		updates: make(chan update, 16),
		palette: defaultPalette(),
	}
	u.th.Shaper = text.NewShaper(text.WithCollection(gofont.Collection()))
	u.th.Palette.Bg = bg
	u.th.Palette.Fg = fg
	u.lyricsList.Axis = layout.Vertical
	u.lyricsList.Gap = 26

	go func() {
		w := new(app.Window)
		w.Option(
			app.Title("BlueRelief"),
			app.Size(unit.Dp(float32(winW)), unit.Dp(float32(winH))),
			app.Decorated(false),
		)
		if err := u.run(context.Background(), w); err != nil {
			log.Printf("bluerelief-gio: %v", err)
		}
		os.Exit(0)
	}()
	app.Main()
}

func (u *ui) run(ctx context.Context, w *app.Window) error {
	go u.fetchInitial(ctx, w)
	go u.streamEvents(ctx, w)
	go u.pollAuth(ctx, w)
	go u.clock(ctx, w)

	for {
		e := w.Event()
		switch e := e.(type) {
		case app.DestroyEvent:
			return e.Err
		case app.FrameEvent:
			u.drainUpdates(w)
			gtx := app.NewContext(&u.ops, e)
			u.layout(gtx)
			e.Frame(gtx.Ops)
		}
	}
}

func (u *ui) drainUpdates(w *app.Window) {
	for {
		select {
		case up := <-u.updates:
			if up.state != nil {
				u.state = up.state
				nextCover := firstCover(up.state)
				if nextCover != "" && nextCover != u.coverURL {
					u.coverURL = nextCover
					go u.loadCover(nextCover, w)
				}
				if nextCover == "" {
					u.coverURL = ""
					u.hasCover = false
					u.hasDisc = false
					u.hasBg = false
					u.palette = defaultPalette()
				}
			}
			if up.auth != nil {
				u.auth = *up.auth
			}
			if up.cover != nil && up.coverURL == u.coverURL {
				u.coverOp = paint.NewImageOp(up.cover)
				u.hasCover = true
				if up.disc != nil {
					u.discOp = paint.NewImageOp(up.disc)
					u.hasDisc = true
				}
				if up.bg != nil {
					u.bgOp = paint.NewImageOp(up.bg)
					u.hasBg = true
				}
				if up.palette != nil {
					u.palette = *up.palette
				}
			}
		default:
			return
		}
	}
}

func (u *ui) layout(gtx layout.Context) layout.Dimensions {
	if forcePPDP > 0 {
		gtx.Metric.PxPerDp = forcePPDP
		gtx.Metric.PxPerSp = forcePPDP
	}
	if dbg {
		dt := gtx.Now.Sub(u.dbgLast)
		u.dbgLast = gtx.Now
		log.Printf("GIO dt=%dms ppdp=%.2f sp=%.2f playing=%t vinyl=%t", dt.Milliseconds(), gtx.Metric.PxPerDp, gtx.Metric.PxPerSp, u.readPlaying(gtx.Now), u.vinylMode)
	}
	u.handleControls(gtx)
	u.background(gtx)

	dims := layout.Inset{
		Top: unit.Dp(32), Right: unit.Dp(40), Bottom: unit.Dp(16), Left: unit.Dp(40),
	}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Gap: gtx.Dp(unit.Dp(48))}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return u.cover(gtx)
			}),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				if len(lyricLines(u.state)) == 0 {
					return layout.Flex{Axis: layout.Vertical, Gap: gtx.Dp(unit.Dp(18))}.Layout(gtx,
						layout.Flexed(1, empty),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.meta(gtx) }),
						layout.Flexed(1, empty),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.controls(gtx) }),
					)
				}
				return layout.Flex{Axis: layout.Vertical, Gap: gtx.Dp(unit.Dp(18))}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.meta(gtx) }),
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions { return u.lyrics(gtx) }),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.controls(gtx) }),
				)
			}),
		)
	})

	u.powerButton(gtx)
	if u.powerConfirm {
		u.powerDialog(gtx)
	}

	// Drive the animation ticker dynamically: while playing, we only need
	// steady high-frequency frames if lyrics are actively scrolling.
	isPlaying := u.readPlaying(gtx.Now)
	needsAnim := isPlaying && (u.lyricScroll != u.lyricTarget)
	u.anim.Store(needsAnim)

	return dims
}

func (u *ui) background(gtx layout.Context) {
	size := gtx.Constraints.Max
	fill(gtx.Ops, image.Rectangle{Max: size}, bg)
	if !u.hasBg {
		return
	}
	gtx.Constraints = layout.Exact(size)
	widget.Image{Src: u.bgOp, Fit: widget.Cover, Position: layout.Center}.Layout(gtx)
}

func (u *ui) handleControls(gtx layout.Context) {
	for u.coverClick.Clicked(gtx) {
		u.vinylMode = !u.vinylMode
		u.vinylLast = time.Time{}
	}
	for u.power.Clicked(gtx) {
		u.powerConfirm = true
	}
	for u.powerNo.Clicked(gtx) {
		u.powerConfirm = false
	}
	for u.powerScrim.Clicked(gtx) {
		u.powerConfirm = false
	}
	for u.powerYes.Clicked(gtx) {
		u.powerConfirm = false
		u.powerOff()
	}
	if u.state == nil {
		return
	}
	if u.can("transport") {
		for u.play.Clicked(gtx) {
			next := !u.readPlaying(gtx.Now)
			u.isPlaying = &optimisticValue[bool]{value: next, until: gtx.Now.Add(2500 * time.Millisecond)}
			if next {
				u.post(u.controlPath("/api/control/play"))
			} else {
				u.post(u.controlPath("/api/control/pause"))
			}
		}
		for u.prev.Clicked(gtx) {
			u.post(u.controlPath("/api/control/previous"))
		}
		for u.next.Clicked(gtx) {
			u.post(u.controlPath("/api/control/next"))
		}
	}
	if u.can("volume") {
		for u.volDown.Clicked(gtx) {
			u.setVolume(gtx.Now, -5)
		}
		for u.volUp.Clicked(gtx) {
			u.setVolume(gtx.Now, 5)
		}
	}
}

func (u *ui) cover(gtx layout.Context) layout.Dimensions {
	size := min(gtx.Constraints.Max.X, gtx.Constraints.Max.Y)
	return u.coverClick.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints = layout.Exact(image.Pt(size, size))
		if u.vinylMode && u.hasCover {
			return u.vinylCover(gtx, size)
		}
		return u.squareCover(gtx, size)
	})
}

func (u *ui) squareCover(gtx layout.Context, size int) layout.Dimensions {
	defer clip.UniformRRect(image.Rectangle{Max: image.Pt(size, size)}, gtx.Dp(unit.Dp(18))).Push(gtx.Ops).Pop()
	fill(gtx.Ops, image.Rectangle{Max: image.Pt(size, size)}, panel)
	if u.hasCover {
		return widget.Image{Src: u.coverOp, Fit: widget.Cover, Position: layout.Center}.Layout(gtx)
	}
	return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		lbl := material.Label(u.th, unit.Sp(54), "BlueRelief")
		lbl.Color = muted
		lbl.Alignment = text.Middle
		return lbl.Layout(gtx)
	})
}

func (u *ui) vinylCover(gtx layout.Context, size int) layout.Dimensions {
	bounds := image.Rectangle{Max: image.Pt(size, size)}
	center := f32.Pt(float32(size)/2, float32(size)/2)
	shadowInset := gtx.Dp(unit.Dp(10))
	fillRound(gtx.Ops, bounds.Inset(shadowInset), color.NRGBA{R: 0, G: 0, B: 0, A: 96}, (size-shadowInset*2)/2)

	disc := bounds.Inset(gtx.Dp(unit.Dp(18)))
	fillEllipse(gtx.Ops, disc, mixColor(u.palette.Accent, color.NRGBA{A: 255}, 0.86))
	drawSpinningDisc(gtx, u.discOp, disc, center, float32(u.vinylAngle))

	ringColor := color.NRGBA{R: 255, G: 255, B: 255, A: 42}
	fillEllipseStroke(gtx.Ops, disc.Inset(gtx.Dp(unit.Dp(42))), ringColor, gtx.Dp(unit.Dp(2)))
	fillEllipseStroke(gtx.Ops, disc.Inset(gtx.Dp(unit.Dp(92))), ringColor, gtx.Dp(unit.Dp(2)))
	fillEllipseStroke(gtx.Ops, disc.Inset(gtx.Dp(unit.Dp(150))), ringColor, gtx.Dp(unit.Dp(2)))

	labelSize := gtx.Dp(unit.Dp(190))
	labelRect := image.Rect(size/2-labelSize/2, size/2-labelSize/2, size/2+labelSize/2, size/2+labelSize/2)
	fillEllipse(gtx.Ops, labelRect, withAlpha(u.palette.Accent, 220))
	holeSize := gtx.Dp(unit.Dp(34))
	holeRect := image.Rect(size/2-holeSize/2, size/2-holeSize/2, size/2+holeSize/2, size/2+holeSize/2)
	fillEllipse(gtx.Ops, holeRect, color.NRGBA{R: 3, G: 3, B: 5, A: 255})

	u.tonearm(gtx, size)
	return layout.Dimensions{Size: image.Pt(size, size)}
}

// drawSpinningDisc paints a pre-masked circular album texture rotated about its
// centre. Because the texture is already a circle (transparent corners baked in
// by makeDisc) there is no clip op: a circle is rotation-invariant, so the disc
// stays full and round at every angle. This avoids the clip.Ellipse + rotation
// combination that flashes black tiles on the board's Panfrost driver.
func drawSpinningDisc(gtx layout.Context, img paint.ImageOp, disc image.Rectangle, center f32.Point, angle float32) {
	src := img.Size()
	if src.X <= 0 || src.Y <= 0 || disc.Empty() {
		return
	}
	// Cover the disc with a hair of overscan so the texture's anti-aliased rim
	// sits just outside the visible vinyl edge.
	diameter := float64(max(disc.Dx(), disc.Dy())) + float64(gtx.Dp(unit.Dp(2)))
	scale := diameter / float64(src.X)
	scaled := float32(src.X) * float32(scale)
	offset := f32.Pt(center.X-scaled/2, center.Y-scaled/2)

	imgStack := op.Affine(f32.AffineId().Scale(f32.Point{}, f32.Pt(float32(scale), float32(scale))).Offset(offset)).Push(gtx.Ops)
	img.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	imgStack.Pop()
	fillEllipse(gtx.Ops, disc, color.NRGBA{R: 0, G: 0, B: 0, A: 58})
}

func (u *ui) advanceVinyl(now time.Time) {
	if !u.vinylMode {
		u.vinylLast = time.Time{}
		return
	}
	if u.vinylLast.IsZero() {
		u.vinylLast = now
		return
	}
	if u.readPlaying(now) {
		delta := now.Sub(u.vinylLast)
		u.vinylAngle = math.Mod(u.vinylAngle+delta.Seconds()*2*math.Pi/vinylTurn.Seconds(), 2*math.Pi)
	}
	u.vinylLast = now
}

func (u *ui) tonearm(gtx layout.Context, size int) {
	ops := gtx.Ops
	pivot := image.Pt(size-gtx.Dp(unit.Dp(115)), gtx.Dp(unit.Dp(92)))
	fillEllipse(ops, image.Rect(pivot.X-gtx.Dp(unit.Dp(34)), pivot.Y-gtx.Dp(unit.Dp(34)), pivot.X+gtx.Dp(unit.Dp(34)), pivot.Y+gtx.Dp(unit.Dp(34))), color.NRGBA{R: 230, G: 230, B: 236, A: 170})
	fillEllipse(ops, image.Rect(pivot.X-gtx.Dp(unit.Dp(18)), pivot.Y-gtx.Dp(unit.Dp(18)), pivot.X+gtx.Dp(unit.Dp(18)), pivot.Y+gtx.Dp(unit.Dp(18))), color.NRGBA{R: 24, G: 24, B: 30, A: 230})

	var arm clip.Path
	arm.Begin(ops)
	arm.MoveTo(f32.Pt(float32(pivot.X), float32(pivot.Y)))
	arm.LineTo(f32.Pt(float32(size)*0.66, float32(size)*0.28))
	arm.LineTo(f32.Pt(float32(size)*0.56, float32(size)*0.48))
	paint.FillShape(ops, color.NRGBA{R: 238, G: 238, B: 242, A: 190}, clip.Stroke{Path: arm.End(), Width: float32(gtx.Dp(unit.Dp(9)))}.Op())

	var head clip.Path
	head.Begin(ops)
	head.MoveTo(f32.Pt(float32(size)*0.53, float32(size)*0.49))
	head.LineTo(f32.Pt(float32(size)*0.60, float32(size)*0.51))
	head.LineTo(f32.Pt(float32(size)*0.58, float32(size)*0.57))
	head.LineTo(f32.Pt(float32(size)*0.51, float32(size)*0.55))
	head.Close()
	paint.FillShape(ops, color.NRGBA{R: 22, G: 22, B: 28, A: 235}, clip.Outline{Path: head.End()}.Op())
}

func (u *ui) meta(gtx layout.Context) layout.Dimensions {
	return layout.Inset{Bottom: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical, Gap: gtx.Dp(unit.Dp(12))}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.statusRow(gtx) }),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Label(u.th, unit.Sp(titleSize(u.state)), trackTitle(u.state))
				lbl.Color = fg
				lbl.MaxLines = titleLines(u.state)
				lbl.Truncator = "..."
				return lbl.Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Label(u.th, unit.Sp(24), subtitle(u.state))
				lbl.Color = muted
				lbl.MaxLines = 1
				lbl.Truncator = "..."
				return lbl.Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.progress(gtx) }),
		)
	})
}

func (u *ui) statusRow(gtx layout.Context) layout.Dimensions {
	return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle, Gap: gtx.Dp(unit.Dp(14))}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			c := disconnected
			if u.readPlaying(gtx.Now) {
				c = u.palette.Accent
			} else if u.state != nil && statusKey(u.state) == "paused" {
				c = paused
			}
			size := gtx.Dp(unit.Dp(16))
			fillRound(gtx.Ops, image.Rectangle{Max: image.Pt(size, size)}, c, size/2)
			return layout.Dimensions{Size: image.Pt(size, size)}
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			lbl := material.Label(u.th, unit.Sp(18), u.sourceStatusText())
			lbl.Color = fg
			return lbl.Layout(gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if !u.spotifyAuthRequired() {
				return layout.Dimensions{}
			}
			lbl := material.Label(u.th, unit.Sp(14), "RUN BLUERELIEF-AUTH")
			lbl.Color = paused
			return lbl.Layout(gtx)
		}),
	)
}

func (u *ui) sourceStatusText() string {
	source := "SPOTIFY"
	if u.state != nil {
		source = u.state.SourceLabel()
	}
	return source + " / " + strings.ToUpper(statusText(u.state))
}

func (u *ui) sourceKind() string {
	if u.state == nil {
		return "spotify"
	}
	return u.state.SourceKind()
}

func (u *ui) spotifyAuthRequired() bool {
	return u.sourceKind() == "spotify" && !u.auth.Authorized
}

func (u *ui) can(capability string) bool {
	if u.state == nil {
		return false
	}
	switch u.sourceKind() {
	case "idle":
		return false
	case "spotify":
		if !u.auth.Authorized {
			return false
		}
	case "airplay":
		if u.auth.AirplayControl != nil && !*u.auth.AirplayControl {
			return false
		}
	}
	return u.state.Can(capability)
}

func (u *ui) controlPath(path string) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + "source=" + url.QueryEscape(u.sourceKind())
}

func (u *ui) progress(gtx layout.Context) layout.Dimensions {
	for u.seek.Clicked(gtx) {
		dur := durationMS(u.state)
		if dur <= 0 || u.state == nil || !u.can("seek") {
			continue
		}
		press := 0
		if h := u.seek.History(); len(h) > 0 {
			press = h[len(h)-1].Position.X
		}
		width := max(1, gtx.Constraints.Max.X)
		ms := int(math.Round(float64(clamp(press, 0, width)) / float64(width) * float64(dur)))
		u.seekLocal(ms, gtx.Now)
		u.post(u.controlPath("/api/control/seek?ms=" + fmt.Sprint(ms)))
	}

	return layout.Flex{Axis: layout.Vertical, Gap: gtx.Dp(unit.Dp(10))}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			height := gtx.Dp(unit.Dp(20))
			width := gtx.Constraints.Max.X
			pos := positionMS(u.state, gtx.Now)
			dur := durationMS(u.state)
			filled := 0
			if dur > 0 {
				filled = int(math.Round(float64(clamp(pos, 0, dur)) / float64(dur) * float64(width)))
			}
			return u.seek.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				size := image.Pt(width, height)
				fillRound(gtx.Ops, image.Rectangle{Max: size}, color.NRGBA{R: 255, G: 255, B: 255, A: 72}, height/2)
				if filled > 0 {
					c := fg
					if u.readPlaying(gtx.Now) {
						c = u.palette.Accent
					}
					glowHeight := height + gtx.Dp(unit.Dp(10))
					glowTop := (height - glowHeight) / 2
					fillRound(gtx.Ops, image.Rect(0, glowTop, filled, glowTop+glowHeight), withAlpha(c, 56), glowHeight/2)
					fillRound(gtx.Ops, image.Rectangle{Max: image.Pt(filled, height)}, c, height/2)
				}
				return layout.Dimensions{Size: size}
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Spacing: layout.SpaceBetween}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return smallLabel(u.th, formatMS(positionMS(u.state, gtx.Now)), fg).Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					dur := durationMS(u.state)
					text := "-:--"
					if dur > 0 {
						text = formatMS(dur)
					}
					return smallLabel(u.th, text, fg).Layout(gtx)
				}),
			)
		}),
	)
}

func (u *ui) lyrics(gtx layout.Context) layout.Dimensions {
	lines := lyricLines(u.state)
	if len(lines) == 0 {
		u.lyricScrollAt = time.Time{}
		u.lyricTarget = 0
		return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions { return layout.Dimensions{} })
	}
	size := gtx.Constraints.Max
	if size.X <= 0 || size.Y <= 0 {
		return layout.Dimensions{Size: size}
	}
	pos := positionMS(u.state, gtx.Now)
	active := u.stableLyricLine(lines, activeLyric(lines, pos), pos)
	u.advanceLyricScroll(gtx, float64(active))

	defer clip.Rect(image.Rectangle{Max: size}).Push(gtx.Ops).Pop()
	rowStep := gtx.Dp(unit.Dp(164))
	if rowStep <= 0 {
		rowStep = 164
	}
	rowHeight := gtx.Dp(unit.Dp(190))
	if rowHeight <= 0 {
		rowHeight = 190
	}
	centerY := size.Y / 2
	visible := size.Y/rowStep + 5
	start := max(0, active-visible)
	end := min(len(lines), active+visible+1)
	drawn := 0
	for i := start; i < end; i++ {
		rowCenter := centerY + int(math.Round((float64(i)-u.lyricScroll)*float64(rowStep)))
		top := rowCenter - rowHeight/2
		if top > size.Y || top+rowHeight < 0 {
			continue
		}
		drawn++

		stack := op.Offset(image.Pt(0, top)).Push(gtx.Ops)
		rowGtx := gtx
		rowGtx.Constraints = layout.Exact(image.Pt(size.X, rowHeight))
		line := strings.TrimSpace(lines[i].Text)
		if line == "" {
			line = " "
		}
		emphasis := 1 - clampFloat(math.Abs(float64(i)-u.lyricScroll), 0, 1)
		lineColor := mixColor(lyricMuted, fg, emphasis)
		lineColor.A = lyricAlpha(rowCenter, size.Y, emphasis)
		// Shape every line at one fixed size and weight so the text is never
		// re-laid-out (and never re-wraps) mid-scroll — that reshaping was the
		// jelly wobble. The active line's growth is a GPU transform instead.
		lbl := material.Label(u.th, lyricFontSize, line)
		lbl.Color = lineColor
		lbl.Font.Weight = font.Medium
		lbl.MaxLines = 2
		lbl.LineHeightScale = 1.06
		lbl.Truncator = "..."
		scale := float32(1 + 0.36*emphasis)
		zoom := op.Affine(f32.AffineId().Scale(f32.Pt(0, float32(rowHeight)/2), f32.Pt(scale, scale))).Push(gtx.Ops)
		layoutLyricLabel(rowGtx, lbl, uint8(math.Round(168+52*emphasis)))
		zoom.Pop()
		stack.Pop()
	}
	if dbg {
		log.Printf("LYR boxH=%d step=%d rowH=%d cy=%d act=%d scr=%.3f drawn=%d total=%d", size.Y, rowStep, rowHeight, centerY, active, u.lyricScroll, drawn, len(lines))
	}
	return layout.Dimensions{Size: size}
}

func layoutLyricLabel(gtx layout.Context, lbl material.LabelStyle, shadowAlpha uint8) layout.Dimensions {
	shadow := lbl
	shadow.Color = color.NRGBA{R: 0, G: 0, B: 0, A: shadowAlpha}

	soft := op.Offset(image.Pt(gtx.Dp(unit.Dp(1)), gtx.Dp(unit.Dp(3)))).Push(gtx.Ops)
	layout.W.Layout(gtx, shadow.Layout)
	soft.Pop()

	return layout.W.Layout(gtx, lbl.Layout)
}

// stableLyricLine commits the active lyric line with hysteresis. Forward moves
// are accepted immediately; a backward move is only honoured when it's a real
// seek (two or more lines, or the position drops well before the committed
// line's start). This absorbs the small backward steps that occur when the
// extrapolated clock snaps to an authoritative state update, which would
// otherwise flip the active line across a boundary and wobble the scroll.
func (u *ui) stableLyricLine(lines []lyrics.Line, raw, pos int) int {
	trackID := ""
	if u.state != nil && u.state.Track != nil {
		trackID = u.state.Track.ID
	}
	if u.lyricScrollAt.IsZero() || trackID != u.lyricTrackID {
		u.lyricLine = raw
		return raw
	}
	if raw > u.lyricLine {
		u.lyricLine = raw
	} else if raw < u.lyricLine {
		idx := clamp(u.lyricLine, 0, len(lines)-1)
		if u.lyricLine-raw >= 2 || pos < lines[idx].TimeMS-lyricBackHysteresisMS {
			u.lyricLine = raw
		}
	}
	return u.lyricLine
}

// advanceLyricScroll eases the displayed scroll position toward the active line
// so the list glides between lines instead of snapping a full row each change.
// It snaps (no animation) on the first frame, on track changes, and on large
// jumps (seeks), and requests follow-up frames until the glide settles.
func (u *ui) advanceLyricScroll(gtx layout.Context, target float64) {
	trackID := ""
	if u.state != nil && u.state.Track != nil {
		trackID = u.state.Track.ID
	}
	u.lyricScroll = target
	u.lyricTarget = target
	u.lyricScrollAt = gtx.Now
	u.lyricTrackID = trackID
}

func (u *ui) controls(gtx layout.Context) layout.Dimensions {
	transport := u.can("transport")
	volume := u.can("volume")
	return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle, Gap: gtx.Dp(unit.Dp(14))}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.iconButton(gtx, &u.prev, iconPrev, false, transport)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			icon := iconPlay
			if u.readPlaying(gtx.Now) {
				icon = iconPause
			}
			return u.iconButton(gtx, &u.play, icon, true, transport)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.iconButton(gtx, &u.next, iconNext, false, transport)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return layout.Dimensions{Size: image.Pt(gtx.Constraints.Min.X, 1)}
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.textButton(gtx, &u.volDown, "-", false, volume) }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			lbl := material.Label(u.th, unit.Sp(26), u.volumeText(gtx.Now))
			lbl.Color = fg
			return layout.Center.Layout(gtx, lbl.Layout)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.textButton(gtx, &u.volUp, "+", false, volume) }),
	)
}

type iconKind int

const (
	iconPrev iconKind = iota
	iconPlay
	iconPause
	iconNext
)

func (u *ui) iconButton(gtx layout.Context, b *widget.Clickable, icon iconKind, primary bool, enabled bool) layout.Dimensions {
	return u.controlButton(gtx, b, primary, enabled, func(gtx layout.Context, c color.NRGBA) {
		drawIcon(gtx, icon, c)
	})
}

func (u *ui) textButton(gtx layout.Context, b *widget.Clickable, txt string, primary bool, enabled bool) layout.Dimensions {
	return u.controlButton(gtx, b, primary, enabled, func(gtx layout.Context, c color.NRGBA) {
		lbl := material.Label(u.th, unit.Sp(44), txt)
		lbl.Color = c
		lbl.Alignment = text.Middle
		lbl.Layout(gtx)
	})
}

func (u *ui) controlButton(gtx layout.Context, b *widget.Clickable, primary bool, enabled bool, content func(layout.Context, color.NRGBA)) layout.Dimensions {
	if !enabled {
		gtx = gtx.Disabled()
	}
	w, h, radius := gtx.Dp(unit.Dp(108)), gtx.Dp(unit.Dp(108)), gtx.Dp(unit.Dp(22))
	bgColor, iconColor := controlBg, fg
	if primary {
		w, h, radius = gtx.Dp(unit.Dp(132)), gtx.Dp(unit.Dp(132)), gtx.Dp(unit.Dp(66))
		bgColor = mixColor(u.palette.Accent, paused, 0.28)
		iconColor = bestInk(bgColor)
		if u.readPlaying(gtx.Now) {
			bgColor = u.palette.Accent
			iconColor = u.palette.Ink
		}
	}
	return b.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		size := image.Pt(w, h)
		gtx.Constraints = layout.Exact(size)
		if b.Pressed() {
			bgColor = scaleColor(bgColor, 0.72)
		}
		if primary {
			fillEllipse(gtx.Ops, image.Rectangle{Max: size}, withAlpha(bgColor, 72))
			fillEllipse(gtx.Ops, image.Rectangle{Max: size}.Inset(gtx.Dp(unit.Dp(5))), bgColor)
		} else {
			fillRound(gtx.Ops, image.Rectangle{Max: size}, bgColor, radius)
		}
		layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			iconSize := gtx.Dp(unit.Dp(54))
			if primary {
				iconSize = gtx.Dp(unit.Dp(70))
			}
			gtx.Constraints = layout.Exact(image.Pt(iconSize, iconSize))
			content(gtx, iconColor)
			return layout.Dimensions{Size: image.Pt(iconSize, iconSize)}
		})
		return layout.Dimensions{Size: size}
	})
}

// powerButton is a top-right overlay that shuts the board down on tap. It is
// drawn last so its pointer area sits above the rest of the UI, and it is not
// gated on auth — it's a hardware control, not a Spotify one.
func (u *ui) powerButton(gtx layout.Context) layout.Dimensions {
	return layout.Inset{Top: unit.Dp(24), Right: unit.Dp(28)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.NE.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return u.power.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				size := image.Pt(gtx.Dp(unit.Dp(64)), gtx.Dp(unit.Dp(64)))
				gtx.Constraints = layout.Exact(size)
				bgColor := color.NRGBA{R: 36, G: 36, B: 42, A: 170}
				iconColor := color.NRGBA{R: 255, G: 138, B: 138, A: 235}
				if u.power.Pressed() {
					bgColor = color.NRGBA{R: 214, G: 64, B: 64, A: 235}
					iconColor = fg
				}
				fillEllipse(gtx.Ops, image.Rectangle{Max: size}, bgColor)
				layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					iconSize := image.Pt(gtx.Dp(unit.Dp(32)), gtx.Dp(unit.Dp(32)))
					gtx.Constraints = layout.Exact(iconSize)
					drawPowerIcon(gtx, iconColor)
					return layout.Dimensions{Size: iconSize}
				})
				return layout.Dimensions{Size: size}
			})
		})
	})
}

// drawPowerIcon paints the IEC power glyph: a ring broken at the top with a
// vertical bar through the gap. Built from short line segments so it renders
// the same on the board's Panfrost driver as it does locally.
func drawPowerIcon(gtx layout.Context, c color.NRGBA) {
	size := gtx.Constraints.Max
	w, h := float32(size.X), float32(size.Y)
	cx, cy := w/2, h*0.54
	radius := w * 0.34
	stroke := float32(gtx.Dp(unit.Dp(4)))

	const halfGap = 0.85 // radians of the gap centred on the top of the ring
	const steps = 28
	startA := -math.Pi/2 + halfGap
	endA := -math.Pi/2 + 2*math.Pi - halfGap
	var ring clip.Path
	ring.Begin(gtx.Ops)
	for i := 0; i <= steps; i++ {
		a := startA + (endA-startA)*float64(i)/float64(steps)
		px := cx + radius*float32(math.Cos(a))
		py := cy + radius*float32(math.Sin(a))
		if i == 0 {
			ring.MoveTo(f32.Pt(px, py))
		} else {
			ring.LineTo(f32.Pt(px, py))
		}
	}
	paint.FillShape(gtx.Ops, c, clip.Stroke{Path: ring.End(), Width: stroke}.Op())

	var bar clip.Path
	bar.Begin(gtx.Ops)
	bar.MoveTo(f32.Pt(cx, cy-radius*1.12))
	bar.LineTo(f32.Pt(cx, cy-radius*0.18))
	paint.FillShape(gtx.Ops, c, clip.Stroke{Path: bar.End(), Width: stroke}.Op())
}

func (u *ui) powerOff() {
	go func() {
		if err := exec.Command("sudo", "poweroff").Run(); err != nil {
			log.Printf("poweroff: %v", err)
		}
	}()
}

// powerDialog is the confirmation modal. The scrim is a full-screen clickable
// drawn first so it dims the UI and absorbs taps (tapping it cancels); the
// dialog box is drawn on top so its buttons win hit-testing.
func (u *ui) powerDialog(gtx layout.Context) {
	size := gtx.Constraints.Max
	u.powerScrim.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints = layout.Exact(size)
		fill(gtx.Ops, image.Rectangle{Max: size}, color.NRGBA{A: 196})
		return layout.Dimensions{Size: size}
	})
	gtx.Constraints = layout.Exact(size)
	layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return u.powerDialogBox(gtx)
	})
}

func (u *ui) powerDialogBox(gtx layout.Context) layout.Dimensions {
	width := gtx.Dp(unit.Dp(560))
	gtx.Constraints.Min.X = width
	gtx.Constraints.Max.X = width
	gtx.Constraints.Min.Y = 0
	return layout.Stack{Alignment: layout.Center}.Layout(gtx,
		layout.Expanded(func(gtx layout.Context) layout.Dimensions {
			fillRound(gtx.Ops, image.Rectangle{Max: gtx.Constraints.Min}, color.NRGBA{R: 30, G: 30, B: 36, A: 248}, gtx.Dp(unit.Dp(28)))
			return layout.Dimensions{Size: gtx.Constraints.Min}
		}),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.UniformInset(unit.Dp(40)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical, Alignment: layout.Middle, Gap: gtx.Dp(unit.Dp(36))}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						lbl := material.Label(u.th, unit.Sp(42), "Power off?")
						lbl.Color = fg
						lbl.Alignment = text.Middle
						lbl.Font.Weight = font.Medium
						return lbl.Layout(gtx)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Flex{Axis: layout.Horizontal, Gap: gtx.Dp(unit.Dp(22))}.Layout(gtx,
							layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
								return u.dialogButton(gtx, &u.powerNo, "Cancel", false)
							}),
							layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
								return u.dialogButton(gtx, &u.powerYes, "Power off", true)
							}),
						)
					}),
				)
			})
		}),
	)
}

func (u *ui) dialogButton(gtx layout.Context, b *widget.Clickable, label string, danger bool) layout.Dimensions {
	bgColor := controlBg
	if danger {
		bgColor = color.NRGBA{R: 214, G: 64, B: 64, A: 255}
	}
	return b.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		size := image.Pt(gtx.Constraints.Max.X, gtx.Dp(unit.Dp(96)))
		gtx.Constraints = layout.Exact(size)
		c := bgColor
		if b.Pressed() {
			c = scaleColor(bgColor, 0.72)
		}
		fillRound(gtx.Ops, image.Rectangle{Max: size}, c, gtx.Dp(unit.Dp(20)))
		return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			lbl := material.Label(u.th, unit.Sp(30), label)
			lbl.Color = fg
			lbl.Alignment = text.Middle
			lbl.Font.Weight = font.Medium
			return lbl.Layout(gtx)
		})
	})
}

func (u *ui) readPlaying(now time.Time) bool {
	if u.isPlaying != nil {
		if now.Before(u.isPlaying.until) {
			return u.isPlaying.value
		}
		u.isPlaying = nil
	}
	return u.state != nil && u.state.Playback.IsPlaying
}

func (u *ui) volumeText(now time.Time) string {
	if u.volume != nil {
		if now.Before(u.volume.until) {
			return fmt.Sprintf("%d%%", u.volume.value)
		}
		u.volume = nil
	}
	if u.state == nil || u.state.Settings.VolumePercent == nil {
		return "-"
	}
	return fmt.Sprintf("%d%%", *u.state.Settings.VolumePercent)
}

func (u *ui) setVolume(now time.Time, delta int) {
	cur := 50
	if u.volume != nil && now.Before(u.volume.until) {
		cur = u.volume.value
	} else if u.state != nil && u.state.Settings.VolumePercent != nil {
		cur = *u.state.Settings.VolumePercent
	}
	next := clamp(cur+delta, 0, 100)
	u.volume = &optimisticValue[int]{value: next, until: now.Add(2500 * time.Millisecond)}
	u.post(u.controlPath("/api/control/volume?percent=" + fmt.Sprint(next)))
}

func (u *ui) seekLocal(ms int, now time.Time) {
	if u.state == nil {
		return
	}
	u.state.Playback.PositionMS = &ms
	u.state.Playback.PositionUpdatedAt = now.UTC().Format(time.RFC3339Nano)
}

func (u *ui) fetchInitial(ctx context.Context, w *app.Window) {
	var s snapshot
	if err := u.getJSON(ctx, "/api/state", &s); err != nil {
		log.Printf("state: %v", err)
		return
	}
	u.send(w, update{state: &s})
}

func (u *ui) pollAuth(ctx context.Context, w *app.Window) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		var st authStatus
		if err := u.getJSON(ctx, "/api/auth/status", &st); err == nil {
			u.send(w, update{auth: &st})
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (u *ui) streamEvents(ctx context.Context, w *app.Window) {
	backoff := time.Second
	for {
		err := u.streamOnce(ctx, w)
		if errors.Is(err, context.Canceled) {
			return
		}
		if err != nil {
			log.Printf("events: %v (reconnect in %s)", err, backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (u *ui) streamOnce(ctx context.Context, w *app.Window) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.baseURL+"/api/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := u.sseClient.Do(req)
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
		var s snapshot
		if err := json.Unmarshal([]byte(line[len("data: "):]), &s); err != nil {
			log.Printf("events: bad payload: %v", err)
			continue
		}
		u.send(w, update{state: &s})
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return io.EOF
}

// clock drives all animation. op.InvalidateCmd, which Gio recommends for
// animation, does not reliably wake the event loop under this board's sway
// compositor — only w.Invalidate does — so the vinyl and lyric scroll were
// stuck at the idle 500ms cadence (~2fps, the "jelly" stutter). Here we tick
// at the animation rate and invalidate every tick while playing, falling back
// to the slow refresh when idle so a paused screen stays cheap.
func (u *ui) clock(ctx context.Context, w *app.Window) {
	t := time.NewTicker(animFrameRate)
	defer t.Stop()
	var idle time.Duration
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if u.anim.Load() {
				idle = 0
				w.Invalidate()
				continue
			}
			if idle += animFrameRate; idle >= refreshPeriod {
				idle = 0
				w.Invalidate()
			}
		}
	}
}

func (u *ui) loadCover(coverURL string, w *app.Window) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	reqURL, err := u.resolveCoverURL(coverURL)
	if err != nil {
		log.Printf("cover: url %q: %v", coverURL, err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		log.Printf("cover: request %q: %v", reqURL, err)
		return
	}
	resp, err := u.client.Do(req)
	if err != nil {
		log.Printf("cover: %v", err)
		return
	}
	defer resp.Body.Close()
	img, _, err := image.Decode(resp.Body)
	if err != nil {
		log.Printf("cover: decode: %v", err)
		return
	}
	palette := extractAlbumPalette(img)
	u.send(w, update{
		coverURL: coverURL,
		cover:    img,
		disc:     makeDisc(img),
		bg:       composeBackground(img, palette),
		palette:  &palette,
	})
}

func (u *ui) resolveCoverURL(raw string) (string, error) {
	ref, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if ref.IsAbs() {
		return ref.String(), nil
	}
	base, err := url.Parse(strings.TrimRight(u.baseURL, "/") + "/")
	if err != nil {
		return "", err
	}
	return base.ResolveReference(ref).String(), nil
}

func (u *ui) getJSON(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.baseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s: status %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func (u *ui) post(path string) {
	go func() {
		req, err := http.NewRequest(http.MethodPost, u.baseURL+path, nil)
		if err != nil {
			log.Printf("control: %v", err)
			return
		}
		resp, err := u.client.Do(req)
		if err != nil {
			log.Printf("control %s: %v", path, err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			log.Printf("control %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
		}
	}()
}

func (u *ui) send(w *app.Window, up update) {
	select {
	case u.updates <- up:
	default:
	}
	w.Invalidate()
}

func fill(ops *op.Ops, r image.Rectangle, c color.NRGBA) {
	paint.FillShape(ops, c, clip.Rect(r).Op())
}

func fillRound(ops *op.Ops, r image.Rectangle, c color.NRGBA, radius int) {
	paint.FillShape(ops, c, clip.UniformRRect(r, radius).Op(ops))
}

func fillEllipse(ops *op.Ops, r image.Rectangle, c color.NRGBA) {
	paint.FillShape(ops, c, clip.Ellipse(r).Op(ops))
}

func fillEllipseStroke(ops *op.Ops, r image.Rectangle, c color.NRGBA, width int) {
	if r.Empty() || width <= 0 {
		return
	}
	paint.FillShape(ops, c, clip.Stroke{Path: clip.Ellipse(r).Path(ops), Width: float32(width)}.Op())
}

func withAlpha(c color.NRGBA, a uint8) color.NRGBA {
	c.A = a
	return c
}

func mixColor(a, b color.NRGBA, t float64) color.NRGBA {
	t = clampFloat(t, 0, 1)
	return color.NRGBA{
		R: uint8(math.Round(float64(a.R)*(1-t) + float64(b.R)*t)),
		G: uint8(math.Round(float64(a.G)*(1-t) + float64(b.G)*t)),
		B: uint8(math.Round(float64(a.B)*(1-t) + float64(b.B)*t)),
		A: uint8(math.Round(float64(a.A)*(1-t) + float64(b.A)*t)),
	}
}

func scaleColor(c color.NRGBA, factor float64) color.NRGBA {
	return color.NRGBA{
		R: uint8(clamp(int(math.Round(float64(c.R)*factor)), 0, 255)),
		G: uint8(clamp(int(math.Round(float64(c.G)*factor)), 0, 255)),
		B: uint8(clamp(int(math.Round(float64(c.B)*factor)), 0, 255)),
		A: c.A,
	}
}

func bestInk(c color.NRGBA) color.NRGBA {
	if relativeLuma(c) > 0.46 {
		return color.NRGBA{R: 4, G: 5, B: 6, A: 255}
	}
	return fg
}

func relativeLuma(c color.NRGBA) float64 {
	r := srgbToLinear(float64(c.R) / 255)
	g := srgbToLinear(float64(c.G) / 255)
	b := srgbToLinear(float64(c.B) / 255)
	return 0.2126*r + 0.7152*g + 0.0722*b
}

func srgbToLinear(v float64) float64 {
	if v <= 0.04045 {
		return v / 12.92
	}
	return math.Pow((v+0.055)/1.055, 2.4)
}

func lyricAlpha(centerY, height int, emphasis float64) uint8 {
	if height <= 0 {
		return 255
	}
	fade := 1.0
	margin := float64(height) * 0.18
	if margin > 1 {
		y := float64(centerY)
		if y < margin {
			fade = y / margin
		} else if bottom := float64(height) - margin; y > bottom {
			fade = (float64(height) - y) / margin
		}
	}
	if fade < 0 {
		fade = 0
	}
	if fade > 1 {
		fade = 1
	}
	base := 0.86 + 0.14*clampFloat(emphasis, 0, 1)
	return uint8(math.Round(255 * base * fade))
}

func drawIcon(gtx layout.Context, icon iconKind, c color.NRGBA) {
	size := gtx.Constraints.Max
	w, h := float32(size.X), float32(size.Y)
	switch icon {
	case iconPlay:
		fillTriangle(gtx.Ops, c, f32.Pt(w*0.34, h*0.22), f32.Pt(w*0.34, h*0.78), f32.Pt(w*0.80, h*0.50))
	case iconPause:
		barW := int(w * 0.20)
		gap := int(w * 0.14)
		barH := int(h * 0.58)
		top := int(h * 0.21)
		left := int(w*0.50) - gap/2 - barW
		right := int(w*0.50) + gap/2
		radius := max(2, int(w*0.045))
		fillRound(gtx.Ops, image.Rect(left, top, left+barW, top+barH), c, radius)
		fillRound(gtx.Ops, image.Rect(right, top, right+barW, top+barH), c, radius)
	case iconPrev:
		barW := int(w * 0.10)
		barH := int(h * 0.58)
		top := int(h * 0.21)
		left := int(w * 0.22)
		fillRound(gtx.Ops, image.Rect(left, top, left+barW, top+barH), c, max(2, int(w*0.035)))
		fillTriangle(gtx.Ops, c, f32.Pt(w*0.76, h*0.22), f32.Pt(w*0.34, h*0.50), f32.Pt(w*0.76, h*0.78))
	case iconNext:
		barW := int(w * 0.10)
		barH := int(h * 0.58)
		top := int(h * 0.21)
		right := int(w*0.78) - barW
		fillTriangle(gtx.Ops, c, f32.Pt(w*0.24, h*0.22), f32.Pt(w*0.24, h*0.78), f32.Pt(w*0.66, h*0.50))
		fillRound(gtx.Ops, image.Rect(right, top, right+barW, top+barH), c, max(2, int(w*0.035)))
	}
}

func fillTriangle(ops *op.Ops, c color.NRGBA, a, b, d f32.Point) {
	var p clip.Path
	p.Begin(ops)
	p.MoveTo(a)
	p.LineTo(b)
	p.LineTo(d)
	p.Close()
	paint.FillShape(ops, c, clip.Outline{Path: p.End()}.Op())
}

type palettePoint struct {
	r, g, b float64
	h, s, l float64
	weight  float64
	score   float64
}

type paletteCluster struct {
	r, g, b float64
	h, s, l float64
	weight  float64
	score   float64
}

func defaultPalette() albumPalette {
	return albumPalette{
		Accent:    accent,
		Secondary: color.NRGBA{R: 68, G: 142, B: 255, A: 255},
		Ink:       bestInk(accent),
	}
}

func extractAlbumPalette(src image.Image) albumPalette {
	const sampleMax = 72
	bounds := src.Bounds()
	if bounds.Dx() <= 0 || bounds.Dy() <= 0 {
		return defaultPalette()
	}
	stepX := max(1, bounds.Dx()/sampleMax)
	stepY := max(1, bounds.Dy()/sampleMax)
	points := make([]palettePoint, 0, sampleMax*sampleMax)
	for y := bounds.Min.Y; y < bounds.Max.Y; y += stepY {
		for x := bounds.Min.X; x < bounds.Max.X; x += stepX {
			c := color.NRGBAModel.Convert(src.At(x, y)).(color.NRGBA)
			if c.A < 192 {
				continue
			}
			r := float64(c.R) / 255
			g := float64(c.G) / 255
			bl := float64(c.B) / 255
			h, s, l := rgbToHSL(r, g, bl)
			if s < 0.10 || l < 0.08 || l > 0.93 {
				continue
			}
			brightness := 1 - clampFloat(math.Abs(l-0.54)*1.65, 0, 0.88)
			p := palettePoint{
				r:      r,
				g:      g,
				b:      bl,
				h:      h,
				s:      s,
				l:      l,
				weight: 0.15 + s*1.85*brightness,
				score:  (0.12 + s) * brightness,
			}
			points = append(points, p)
		}
	}
	if len(points) == 0 {
		return defaultPalette()
	}

	sort.Slice(points, func(i, j int) bool {
		return points[i].score > points[j].score
	})
	centers := seedPaletteCenters(points, 5)
	for range 7 {
		clusters := make([]paletteCluster, len(centers))
		for _, p := range points {
			idx := nearestPaletteCenter(p, centers)
			clusters[idx].r += p.r * p.weight
			clusters[idx].g += p.g * p.weight
			clusters[idx].b += p.b * p.weight
			clusters[idx].weight += p.weight
		}
		for i := range clusters {
			if clusters[i].weight == 0 {
				continue
			}
			r := clusters[i].r / clusters[i].weight
			g := clusters[i].g / clusters[i].weight
			bl := clusters[i].b / clusters[i].weight
			h, s, l := rgbToHSL(r, g, bl)
			centers[i] = palettePoint{r: r, g: g, b: bl, h: h, s: s, l: l, weight: clusters[i].weight}
		}
	}

	clusters := scorePaletteClusters(points, centers)
	sort.Slice(clusters, func(i, j int) bool {
		return clusters[i].score > clusters[j].score
	})
	if len(clusters) == 0 {
		return defaultPalette()
	}
	accentColor := polishPaletteColor(clusters[0], true)
	secondaryColor := accentColor
	for _, c := range clusters[1:] {
		if hueDistance(c.h, clusters[0].h) >= 20 {
			secondaryColor = polishPaletteColor(c, false)
			break
		}
	}
	if secondaryColor == accentColor && len(clusters) > 1 {
		secondaryColor = polishPaletteColor(clusters[1], false)
	}
	return albumPalette{
		Accent:    accentColor,
		Secondary: secondaryColor,
		Ink:       bestInk(accentColor),
	}
}

func seedPaletteCenters(points []palettePoint, k int) []palettePoint {
	centers := make([]palettePoint, 0, k)
	for _, p := range points {
		if len(centers) == 0 || distinctPalettePoint(p, centers) {
			centers = append(centers, p)
			if len(centers) == k {
				return centers
			}
		}
	}
	for i := 0; len(centers) < k && i < len(points); i += max(1, len(points)/k) {
		centers = append(centers, points[i])
	}
	return centers
}

func distinctPalettePoint(p palettePoint, centers []palettePoint) bool {
	for _, c := range centers {
		if hueDistance(p.h, c.h) < 18 || rgbDistance(p, c) < 0.16 {
			return false
		}
	}
	return true
}

func nearestPaletteCenter(p palettePoint, centers []palettePoint) int {
	bestIdx := 0
	bestDist := math.MaxFloat64
	for i, c := range centers {
		dist := rgbDistance(p, c)
		if dist < bestDist {
			bestDist = dist
			bestIdx = i
		}
	}
	return bestIdx
}

func scorePaletteClusters(points []palettePoint, centers []palettePoint) []paletteCluster {
	clusters := make([]paletteCluster, len(centers))
	for _, p := range points {
		idx := nearestPaletteCenter(p, centers)
		clusters[idx].r += p.r * p.weight
		clusters[idx].g += p.g * p.weight
		clusters[idx].b += p.b * p.weight
		clusters[idx].weight += p.weight
	}
	out := make([]paletteCluster, 0, len(clusters))
	for _, c := range clusters {
		if c.weight == 0 {
			continue
		}
		c.r /= c.weight
		c.g /= c.weight
		c.b /= c.weight
		c.h, c.s, c.l = rgbToHSL(c.r, c.g, c.b)
		brightness := 1 - clampFloat(math.Abs(c.l-0.54)*1.7, 0, 0.9)
		c.score = c.weight * (0.16 + c.s) * brightness
		out = append(out, c)
	}
	return out
}

func polishPaletteColor(c paletteCluster, primary bool) color.NRGBA {
	s := clampFloat(c.s, 0.52, 0.94)
	l := c.l
	if primary {
		l = clampFloat(l, 0.42, 0.64)
		if l < 0.50 && c.s > 0.48 {
			l = 0.50
		}
	} else {
		s = clampFloat(c.s, 0.38, 0.82)
		l = clampFloat(l, 0.36, 0.58)
	}
	r, g, b := hslToRGB(c.h, s, l)
	return color.NRGBA{
		R: uint8(clamp(int(math.Round(r*255)), 0, 255)),
		G: uint8(clamp(int(math.Round(g*255)), 0, 255)),
		B: uint8(clamp(int(math.Round(b*255)), 0, 255)),
		A: 255,
	}
}

func rgbDistance(a, b palettePoint) float64 {
	dr := a.r - b.r
	dg := a.g - b.g
	db := a.b - b.b
	return dr*dr + dg*dg + db*db
}

func hueDistance(a, b float64) float64 {
	d := math.Abs(a - b)
	if d > 180 {
		d = 360 - d
	}
	return d
}

func rgbToHSL(r, g, b float64) (h, s, l float64) {
	maxC := math.Max(r, math.Max(g, b))
	minC := math.Min(r, math.Min(g, b))
	l = (maxC + minC) / 2
	if maxC == minC {
		return 0, 0, l
	}
	d := maxC - minC
	if l > 0.5 {
		s = d / (2 - maxC - minC)
	} else {
		s = d / (maxC + minC)
	}
	switch maxC {
	case r:
		h = (g - b) / d
		if g < b {
			h += 6
		}
	case g:
		h = (b-r)/d + 2
	default:
		h = (r-g)/d + 4
	}
	h *= 60
	return h, s, l
}

func hslToRGB(h, s, l float64) (r, g, b float64) {
	h = math.Mod(h, 360)
	if h < 0 {
		h += 360
	}
	if s == 0 {
		return l, l, l
	}
	q := l * (1 + s)
	if l >= 0.5 {
		q = l + s - l*s
	}
	p := 2*l - q
	hk := h / 360
	return hueToRGB(p, q, hk+1.0/3.0), hueToRGB(p, q, hk), hueToRGB(p, q, hk-1.0/3.0)
}

func hueToRGB(p, q, t float64) float64 {
	if t < 0 {
		t++
	}
	if t > 1 {
		t--
	}
	switch {
	case t < 1.0/6.0:
		return p + (q-p)*6*t
	case t < 0.5:
		return q
	case t < 2.0/3.0:
		return p + (q-p)*(2.0/3.0-t)*6
	default:
		return p
	}
}

// composeBackground bakes the entire static backdrop — graded ambient plus
// every scrim and glow gradient — into one image once per track. The draw loop
// then paints a single texture per frame instead of rasterising six full-screen
// gradients, which is what the board's GPU could not sustain while animating.
func composeBackground(src image.Image, palette albumPalette) image.Image {
	const w, h = 320, 180
	dst := makeAmbient(src, w, h)
	scrim := color.NRGBA{R: 5, G: 5, B: 7}
	compositeFill(dst, withAlpha(scrim, 92))

	compositeLinearGradient(dst, image.Rect(w/3, 0, w, h), f32.Pt(float32(w/3), 0), color.NRGBA{}, f32.Pt(w, h), withAlpha(palette.Accent, 46))
	compositeLinearGradient(dst, image.Rect(0, h/2, w, h), f32.Pt(0, h), withAlpha(palette.Secondary, 34), f32.Pt(w, h/2), color.NRGBA{})

	ls := image.Rect(w*43/100, 0, w, h)
	compositeLinearGradient(dst, ls, f32.Pt(float32(ls.Min.X), 0), withAlpha(scrim, 24), f32.Pt(w, 0), withAlpha(scrim, 188))

	top := image.Rect(0, 0, w, h/2)
	compositeLinearGradient(dst, top, f32.Pt(0, 0), withAlpha(scrim, 118), f32.Pt(0, float32(top.Max.Y)), withAlpha(scrim, 0))
	bottom := image.Rect(0, h/2, w, h)
	compositeLinearGradient(dst, bottom, f32.Pt(0, float32(bottom.Min.Y)), withAlpha(scrim, 0), f32.Pt(0, float32(bottom.Max.Y)), withAlpha(scrim, 150))
	left := image.Rect(0, 0, w*3/5, h)
	compositeLinearGradient(dst, left, f32.Pt(0, 0), withAlpha(scrim, 84), f32.Pt(float32(left.Max.X), 0), withAlpha(scrim, 0))
	return dst
}

// makeDisc center-crops the cover to a square and masks it to a circle with a
// soft edge, so it can be spun without a clip op (see drawSpinningDisc).
func makeDisc(src image.Image) image.Image {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	side := min(sw, sh)
	if side <= 0 {
		return image.NewNRGBA(image.Rect(0, 0, 1, 1))
	}
	ox := b.Min.X + (sw-side)/2
	oy := b.Min.Y + (sh-side)/2
	dst := image.NewNRGBA(image.Rect(0, 0, side, side))
	r := float64(side) / 2
	const edge = 1.5
	for y := range side {
		for x := range side {
			dist := math.Hypot(float64(x)+0.5-r, float64(y)+0.5-r)
			a := 1.0
			switch {
			case dist >= r:
				continue
			case dist > r-edge:
				a = (r - dist) / edge
			}
			c := color.NRGBAModel.Convert(src.At(ox+x, oy+y)).(color.NRGBA)
			off := dst.PixOffset(x, y)
			dst.Pix[off] = c.R
			dst.Pix[off+1] = c.G
			dst.Pix[off+2] = c.B
			dst.Pix[off+3] = uint8(math.Round(float64(c.A) * a))
		}
	}
	return dst
}

func compositeFill(dst *image.NRGBA, c color.NRGBA) {
	b := dst.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			alphaOver(dst, x, y, c)
		}
	}
}

func compositeLinearGradient(dst *image.NRGBA, r image.Rectangle, p1 f32.Point, c1 color.NRGBA, p2 f32.Point, c2 color.NRGBA) {
	r = r.Intersect(dst.Bounds())
	if r.Empty() {
		return
	}
	dx := float64(p2.X - p1.X)
	dy := float64(p2.Y - p1.Y)
	denom := dx*dx + dy*dy
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			t := 0.0
			if denom > 0 {
				t = clampFloat(((float64(x)+0.5-float64(p1.X))*dx+(float64(y)+0.5-float64(p1.Y))*dy)/denom, 0, 1)
			}
			alphaOver(dst, x, y, mixColor(c1, c2, t))
		}
	}
}

// alphaOver composites src over an opaque dst pixel (straight-alpha NRGBA).
func alphaOver(dst *image.NRGBA, x, y int, src color.NRGBA) {
	if src.A == 0 {
		return
	}
	off := dst.PixOffset(x, y)
	sa := float64(src.A) / 255
	dst.Pix[off] = uint8(math.Round(float64(src.R)*sa + float64(dst.Pix[off])*(1-sa)))
	dst.Pix[off+1] = uint8(math.Round(float64(src.G)*sa + float64(dst.Pix[off+1])*(1-sa)))
	dst.Pix[off+2] = uint8(math.Round(float64(src.B)*sa + float64(dst.Pix[off+2])*(1-sa)))
	dst.Pix[off+3] = 255
}

func makeAmbient(src image.Image, dstW, dstH int) *image.NRGBA {
	dst := image.NewNRGBA(image.Rect(0, 0, dstW, dstH))
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= 0 || sh <= 0 {
		return dst
	}
	srcAspect := float64(sw) / float64(sh)
	dstAspect := float64(dstW) / float64(dstH)
	cropW, cropH := sw, sh
	if srcAspect > dstAspect {
		cropW = int(math.Round(float64(sh) * dstAspect))
	} else {
		cropH = int(math.Round(float64(sw) / dstAspect))
	}
	srcX := b.Min.X + (sw-cropW)/2
	srcY := b.Min.Y + (sh-cropH)/2
	for y := range dstH {
		for x := range dstW {
			sx := srcX + clamp(x*cropW/dstW, 0, cropW-1)
			sy := srcY + clamp(y*cropH/dstH, 0, cropH-1)
			c := color.NRGBAModel.Convert(src.At(sx, sy)).(color.NRGBA)
			dst.SetNRGBA(x, y, gradeAmbient(c, x, y, dstW, dstH))
		}
	}
	blurNRGBA(dst, 11, 4)
	return dst
}

func gradeAmbient(c color.NRGBA, x, y, w, h int) color.NRGBA {
	r, g, b := float64(c.R), float64(c.G), float64(c.B)
	luma := 0.2126*r + 0.7152*g + 0.0722*b
	const saturation = 1.85
	const gain = 1.08
	r = (luma + (r-luma)*saturation) * gain
	g = (luma + (g-luma)*saturation) * gain
	b = (luma + (b-luma)*saturation) * gain

	nx := (float64(x)/float64(w-1))*2 - 1
	ny := (float64(y)/float64(h-1))*2 - 1
	dist := math.Sqrt(nx*nx + ny*ny)
	vignette := 1 - clampFloat((dist-0.18)/1.15, 0, 1)*0.34
	if y > h*55/100 {
		vignette *= 1 - clampFloat((float64(y)/float64(h)-0.55)/0.45, 0, 1)*0.20
	}
	return color.NRGBA{
		R: uint8(clamp(int(math.Round(r*vignette)), 0, 255)),
		G: uint8(clamp(int(math.Round(g*vignette)), 0, 255)),
		B: uint8(clamp(int(math.Round(b*vignette)), 0, 255)),
		A: c.A,
	}
}

func blurNRGBA(img *image.NRGBA, radius, passes int) {
	if radius <= 0 || passes <= 0 {
		return
	}
	tmp := image.NewNRGBA(img.Bounds())
	for range passes {
		boxBlurHorizontal(img, tmp, radius)
		boxBlurVertical(tmp, img, radius)
	}
}

func boxBlurHorizontal(src, dst *image.NRGBA, radius int) {
	b := src.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			var r, g, bl, a, n int
			for ix := max(b.Min.X, x-radius); ix <= min(b.Max.X-1, x+radius); ix++ {
				off := src.PixOffset(ix, y)
				r += int(src.Pix[off])
				g += int(src.Pix[off+1])
				bl += int(src.Pix[off+2])
				a += int(src.Pix[off+3])
				n++
			}
			off := dst.PixOffset(x, y)
			dst.Pix[off] = uint8(r / n)
			dst.Pix[off+1] = uint8(g / n)
			dst.Pix[off+2] = uint8(bl / n)
			dst.Pix[off+3] = uint8(a / n)
		}
	}
}

func boxBlurVertical(src, dst *image.NRGBA, radius int) {
	b := src.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			var r, g, bl, a, n int
			for iy := max(b.Min.Y, y-radius); iy <= min(b.Max.Y-1, y+radius); iy++ {
				off := src.PixOffset(x, iy)
				r += int(src.Pix[off])
				g += int(src.Pix[off+1])
				bl += int(src.Pix[off+2])
				a += int(src.Pix[off+3])
				n++
			}
			off := dst.PixOffset(x, y)
			dst.Pix[off] = uint8(r / n)
			dst.Pix[off+1] = uint8(g / n)
			dst.Pix[off+2] = uint8(bl / n)
			dst.Pix[off+3] = uint8(a / n)
		}
	}
}

func smallLabel(th *material.Theme, txt string, c color.NRGBA) material.LabelStyle {
	lbl := material.Label(th, unit.Sp(18), txt)
	lbl.Color = c
	return lbl
}

func empty(gtx layout.Context) layout.Dimensions {
	return layout.Dimensions{Size: gtx.Constraints.Min}
}

func statusKey(s *snapshot) string {
	if s == nil || s.Playback.Status == "" || s.Playback.Status == "disconnected" {
		return "disconnected"
	}
	if s.Playback.IsPlaying {
		return "playing"
	}
	return "paused"
}

func statusText(s *snapshot) string {
	if s == nil || strings.TrimSpace(s.Playback.Status) == "" {
		return "Disconnected"
	}
	txt := strings.ReplaceAll(s.Playback.Status, "_", " ")
	return strings.ToUpper(txt[:1]) + txt[1:]
}

func trackTitle(s *snapshot) string {
	if s == nil || s.Track == nil || s.Track.Name == "" {
		return "-"
	}
	return s.Track.Name
}

func subtitle(s *snapshot) string {
	if s == nil || s.Track == nil {
		return "-"
	}
	artist := s.ArtistText()
	if artist == "" {
		artist = "-"
	}
	if s.Track.Album == "" {
		return artist
	}
	return artist + " · " + s.Track.Album
}

func titleSize(s *snapshot) int {
	if len(lyricLines(s)) > 0 {
		return 34
	}
	return 66
}

func titleLines(s *snapshot) int {
	if len(lyricLines(s)) > 0 {
		return 1
	}
	return 3
}

func firstCover(s *snapshot) string {
	if s == nil {
		return ""
	}
	for _, raw := range s.CoverURLs() {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		u, err := url.Parse(trimmed)
		if err != nil {
			continue
		}
		if u.IsAbs() {
			if u.Scheme == "http" || u.Scheme == "https" {
				return trimmed
			}
			continue
		}
		return trimmed
	}
	return ""
}

func positionMS(s *snapshot, now time.Time) int {
	if s == nil {
		return 0
	}
	return s.EstimatedPositionMS(now)
}

func durationMS(s *snapshot) int {
	if s == nil || s.Track == nil || s.Track.DurationMS == nil {
		return 0
	}
	return *s.Track.DurationMS
}

func lyricLines(s *snapshot) []lyrics.Line {
	if s == nil || s.Lyrics == nil || len(s.Lyrics.Lines) == 0 {
		return nil
	}
	return s.Lyrics.Lines
}

func activeLyric(lines []lyrics.Line, pos int) int {
	active := 0
	for i, line := range lines {
		if line.TimeMS <= pos {
			active = i
		} else {
			break
		}
	}
	return active
}

func formatMS(ms int) string {
	if ms < 0 {
		ms = 0
	}
	seconds := ms / 1000
	return fmt.Sprintf("%d:%02d", seconds/60, seconds%60)
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

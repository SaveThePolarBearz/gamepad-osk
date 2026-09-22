package main

import (
	"errors"
	"math"
	"os"
	"sort"
	"strings"
	"time"
	"unsafe"
)

const promptFontPath = "/usr/share/fonts/TTF/promptfont.ttf"

type texCacheKey struct {
	text  string
	font  uintptr // font pointer as identity
	color Color
}

type texCacheEntry struct {
	tex  *Texture
	w, h float32
}

type Renderer struct {
	renderer       *SDLRenderer
	theme          Theme
	unit           int32   // rounded unit (row height / band math)
	unitF          float64 // fractional unit (exact per-row key widths)
	pad            int32
	statusH        int32 // height of modifier status bar
	font           *Font
	fontSmall      *Font
	fontGlyph      *Font
	flashText      string
	flashGlyphText string // rendered with Promptfont, placed before flashText
	flashEnd       time.Time
	texCache       map[texCacheKey]texCacheEntry
	dirtyFrames    int // redraw counter for double/triple buffering
}

func NewRenderer(r *SDLRenderer, theme Theme, unitSizeF float64, padding int32) (*Renderer, error) {
	unitSize := int32(math.Round(unitSizeF))
	fontSize := max32(12, int32(unitSizeF*0.32))
	glyphSize := max32(12, int32(unitSizeF*0.28))

	if err := TTF3Init(); err != nil {
		return nil, err
	}

	fontPath := findFont("DejaVu Sans", "Liberation Sans", "FreeSans")
	if fontPath == "" {
		return nil, errors.New("no suitable font found")
	}

	font, err := TTF3OpenFont(fontPath, float32(fontSize))
	if err != nil {
		return nil, err
	}
	fontSmall, err := TTF3OpenFont(fontPath, float32(max32(10, fontSize-4)))
	if err != nil {
		return nil, err
	}

	var fontGlyph *Font
	if _, err := os.Stat(promptFontPath); err == nil {
		fontGlyph, _ = TTF3OpenFont(promptFontPath, float32(glyphSize))
	}
	if fontGlyph == nil {
		fontGlyph, _ = TTF3OpenFont(fontPath, float32(max32(10, fontSize-6)))
	}

	return &Renderer{
		renderer:    r,
		theme:       theme,
		unit:        unitSize,
		unitF:       unitSizeF,
		pad:         padding,
		statusH:     max32(20, int32(unitSizeF*0.4)),
		font:        font,
		fontSmall:   fontSmall,
		fontGlyph:   fontGlyph,
		texCache:    make(map[texCacheKey]texCacheEntry),
		dirtyFrames: 3, // force initial draw
	}, nil
}

func (r *Renderer) Draw(kb *KeyboardState) {
	if r.dirtyFrames <= 0 {
		return
	}
	// Wayland: don't render until layer-shell configure is received
	if isWayland && !IsLayerShellReady() {
		return
	}
	r.dirtyFrames--

	t := r.theme
	setColor(r.renderer, t.Bg)
	SDL3RenderClear(r.renderer)

	// Draw modifier status bar
	r.drawModifierBar(kb)

	// Draw keyboard rows (flush to the top/edges; only interior gaps padded)
	y := r.statusH
	for ri, row := range kb.Layout {
		wpx, _ := keyRowPix(row, r.unitF, r.pad)
		x := int32(0)
		for ci, key := range row {
			isCursor := ri == kb.CursorRow && ci == kb.CursorCol
			r.drawKey(key, x, y, wpx[ci], isCursor, kb)
			x += wpx[ci] + r.pad
		}
		y += r.unit + r.pad
	}

	// Accent popup
	if kb.AccentPopup != nil {
		r.drawAccentPopup(kb)
	}

	SDL3RenderPresent(r.renderer)
}

// MarkDirty signals that the display needs updating.
// Redraws for 3 frames to cover double/triple buffering.
func (r *Renderer) MarkDirty() {
	r.dirtyFrames = 3
}

// SetScale updates key metrics after a live window resize (output switch) and
// rebuilds the fonts + texture cache for the new unit. Window size alone isn't
// enough: Draw lays keys out with r.unit and renders glyphs with fonts opened
// at the old unit, which would leave keys at their old (tiny) size on a larger
// surface.
func (r *Renderer) SetScale(unitF float64, pad, statusH int32) {
	r.unit = int32(math.Round(unitF))
	r.unitF = unitF
	r.pad = pad
	r.statusH = statusH

	fontSize := max32(12, int32(unitF*0.32))
	glyphSize := max32(12, int32(unitF*0.28))
	fontPath := findFont("DejaVu Sans", "Liberation Sans", "FreeSans")
	if fontPath == "" {
		r.dirtyFrames = 3
		return
	}
	if f, err := TTF3OpenFont(fontPath, float32(fontSize)); err == nil {
		TTF3CloseFont(r.font)
		r.font = f
	}
	if f, err := TTF3OpenFont(fontPath, float32(max32(10, fontSize-4))); err == nil {
		TTF3CloseFont(r.fontSmall)
		r.fontSmall = f
	}
	if _, err := os.Stat(promptFontPath); err == nil {
		if f, err := TTF3OpenFont(promptFontPath, float32(glyphSize)); err == nil {
			TTF3CloseFont(r.fontGlyph)
			r.fontGlyph = f
		}
	} else if f, err := TTF3OpenFont(fontPath, float32(max32(10, fontSize-6))); err == nil {
		TTF3CloseFont(r.fontGlyph)
		r.fontGlyph = f
	}
	// Old textures were rendered for the previous font sizes.
	r.texCache = make(map[texCacheKey]texCacheEntry)
	r.dirtyFrames = 3
}

func (r *Renderer) drawModifierBar(kb *KeyboardState) {
	// Modifier pills (left-aligned)
	var labels []string
	if kb.ShiftActive {
		labels = append(labels, "SHIFT")
	}
	if kb.CapsActive {
		labels = append(labels, "CAPS")
	}
	if kb.CtrlActive {
		labels = append(labels, "CTRL")
	}
	if kb.AltActive {
		labels = append(labels, "ALT")
	}
	if kb.MetaActive {
		labels = append(labels, "SUPER")
	}
	if kb.AltTabHeld {
		labels = append(labels, "ALT-TAB")
	}

	x := int32(0)
	tc := r.theme.ModifierActiveText
	for _, label := range labels {
		r.drawPill(label, x, r.pad, r.theme.ModifierActiveBg, tc)
		tw, _ := TTF3GetStringSize(r.fontSmall, label)
		x += tw + 10 + 6
	}

	// Status flash (right-aligned, 2 seconds)
	if (r.flashText != "" || r.flashGlyphText != "") && time.Now().Before(r.flashEnd) {
		sw, _ := SDL3GetRenderOutputSize(r.renderer)
		rightEdge := float32(sw)

		// Render text part (right-aligned)
		if r.flashText != "" {
			entry := r.cachedText(r.fontSmall, r.flashText, r.theme.KeyText)
			if entry.tex != nil {
				dst := FRect{X: rightEdge - entry.w, Y: float32(r.pad + 2), W: entry.w, H: entry.h}
				SDL3RenderTexture(r.renderer, entry.tex, nil, &dst)
				rightEdge = dst.X - 4
			}
		}

		// Render glyph part (Promptfont, left of text)
		if r.flashGlyphText != "" {
			entry := r.cachedText(r.fontGlyph, r.flashGlyphText, r.theme.KeyText)
			if entry.tex != nil {
				rightEdge = rightEdge - entry.w - 4
				dst := FRect{X: float32(rightEdge), Y: float32(r.pad + 2), W: entry.w, H: entry.h}
				SDL3RenderTexture(r.renderer, entry.tex, nil, &dst)
			}
		}
	}
}

func (r *Renderer) drawPill(label string, x, y int32, bg Color, fg Color) {
	entry := r.cachedText(r.fontSmall, label, fg)
	if entry.tex == nil {
		return
	}
	lw := int32(entry.w) + 10
	lh := int32(entry.h) + 4
	pill := FRect{X: float32(x), Y: float32(y), W: float32(lw), H: float32(lh)}
	setColor(r.renderer, bg)
	SDL3RenderFillRect(r.renderer, pill)
	dst := FRect{X: pill.X + (pill.W-entry.w)/2, Y: pill.Y + (pill.H-entry.h)/2, W: entry.w, H: entry.h}
	SDL3RenderTexture(r.renderer, entry.tex, nil, &dst)
}

// cachedText returns a cached texture entry, rasterizing on first use.
func (r *Renderer) cachedText(font *Font, text string, color Color) texCacheEntry {
	if text == "" || font == nil {
		return texCacheEntry{}
	}
	key := texCacheKey{text: text, font: uintptr(unsafe.Pointer(font)), color: color} //nolint:gosec // G103: pointer used as identity key only, not dereferenced
	if entry, ok := r.texCache[key]; ok {
		return entry
	}
	surf, err := TTF3RenderTextBlended(font, text, color)
	if err != nil {
		return texCacheEntry{}
	}
	tex := SDL3CreateTextureFromSurface(r.renderer, surf)
	w := float32(SDL3SurfaceWidth(surf))
	h := float32(SDL3SurfaceHeight(surf))
	SDL3DestroySurface(surf)
	if tex == nil {
		return texCacheEntry{}
	}
	entry := texCacheEntry{tex: tex, w: w, h: h}
	r.texCache[key] = entry
	return entry
}

func (r *Renderer) drawKey(key KeyDef, x, y, width int32, isCursor bool, kb *KeyboardState) {
	base := FRect{
		X: float32(x), Y: float32(y),
		W: float32(width), H: float32(r.unit),
	}
	rect := base
	x, y = int32(rect.X), int32(rect.Y)
	w, h := int32(rect.W), int32(rect.H)
	t := r.theme

	flashed := kb.IsFlashed(key)
	var bg, border Color
	switch {
	case isCursor || flashed:
		bg, border = t.HighlightBg, t.HighlightBorder
	case key.IsModifier && isModActive(key, kb):
		bg, border = t.ModifierActiveBg, t.HighlightBorder
	case key.IsModifier:
		bg, border = t.ModifierBg, t.KeyBorder
	case strings.HasPrefix(key.Label, "F") && len(key.Label) > 1 && key.Label[1] >= '0' && key.Label[1] <= '9':
		bg, border = t.FnKeyBg, t.KeyBorder
	default:
		bg, border = t.KeyBg, t.KeyBorder
	}

	// Fill
	setColor(r.renderer, bg)
	SDL3RenderFillRect(r.renderer, rect)
	// Border
	setColor(r.renderer, border)
	SDL3RenderRect(r.renderer, rect)

	// Label
	label := kb.DisplayLabel(key)
	tc := t.KeyText
	if isCursor || flashed {
		tc = Color{R: 255, G: 255, B: 255, A: 255}
	} else if key.IsModifier && isModActive(key, kb) {
		tc = t.ModifierActiveText
	}
	// Use Promptfont for labels containing its codepoints (e.g. mouse speed icons)
	labelFont := r.font
	if r.fontGlyph != nil && hasPromptfontRune(label) {
		labelFont = r.fontGlyph
	}
	r.renderText(labelFont, label, tc, rect, AlignCenter)

	// Shift hint (top-right)
	if key.ShiftLabel != "" && !key.IsModifier && !isCursor {
		if kb.ShiftActive == kb.CapsActive {
			hintFont := r.fontSmall
			if r.fontGlyph != nil && hasPromptfontRune(key.ShiftLabel) {
				hintFont = r.fontGlyph
			}
			r.renderText(hintFont, key.ShiftLabel, t.ModifierText,
				FRect{X: float32(x), Y: float32(y), W: float32(w - 4), H: float32(h)}, AlignTopRight)
		}
	}

	// Controller glyph (bottom-right)
	if glyph, ok := KeyGlyphs[key.Code]; ok && r.fontGlyph != nil {
		r.renderText(r.fontGlyph, glyph, t.GlyphColor,
			FRect{X: float32(x), Y: float32(y), W: float32(w - 3), H: float32(h - 2)}, AlignBottomRight)
	}
}

func (r *Renderer) drawAccentPopup(kb *KeyboardState) {
	accents := kb.AccentPopup.Accents
	sel := kb.AccentPopup.Selected
	row := kb.Layout[kb.CursorRow]

	wpx, _ := keyRowPix(row, r.unitF, r.pad)
	xo := int32(0)
	for i := range kb.CursorCol {
		xo += wpx[i] + r.pad
	}
	ky := r.statusH + int32(kb.CursorRow)*(r.unit+r.pad) //nolint:gosec // G115: cursor index fits in int32
	py := ky - r.unit - r.pad

	tw := int32(len(accents))*(r.unit+r.pad) + r.pad //nolint:gosec // G115: accent count fits in int32
	pr := FRect{X: float32(xo), Y: float32(py), W: float32(tw), H: float32(r.unit + r.pad*2)}
	setColor(r.renderer, r.theme.AccentPopupBg)
	SDL3RenderFillRect(r.renderer, pr)
	setColor(r.renderer, r.theme.HighlightBorder)
	SDL3RenderRect(r.renderer, pr)

	ax := xo + r.pad
	for i, accent := range accents {
		ar := FRect{X: float32(ax), Y: float32(py + r.pad), W: float32(r.unit), H: float32(r.unit)}
		if i == sel {
			setColor(r.renderer, r.theme.AccentHighlightBg)
			SDL3RenderFillRect(r.renderer, ar)
		}
		r.renderText(r.font, accent.Label, r.theme.AccentPopupText, ar, AlignCenter)
		ax += r.unit + r.pad
	}
}

type TextAlign int

const (
	AlignCenter TextAlign = iota
	AlignTopRight
	AlignBottomRight
)

func (r *Renderer) renderText(font *Font, text string, color Color, rect FRect, align TextAlign) {
	entry := r.cachedText(font, text, color)
	if entry.tex == nil {
		return
	}

	var dst FRect
	switch align {
	case AlignCenter:
		dst = FRect{
			X: rect.X + (rect.W-entry.w)/2,
			Y: rect.Y + (rect.H-entry.h)/2,
			W: entry.w, H: entry.h,
		}
	case AlignTopRight:
		dst = FRect{
			X: rect.X + rect.W - entry.w,
			Y: rect.Y + 2,
			W: entry.w, H: entry.h,
		}
	case AlignBottomRight:
		dst = FRect{
			X: rect.X + rect.W - entry.w,
			Y: rect.Y + rect.H - entry.h,
			W: entry.w, H: entry.h,
		}
	}
	SDL3RenderTexture(r.renderer, entry.tex, nil, &dst)
}

// KeyAtWindow maps a window pixel point to the keyboard grid, returning the
// (row, col) of the key nearest to it. Points inside a row's vertical band
// always resolve to the nearest column (snapping in the padding gaps and
// clamping at the row edges). Returns ok=false only for points above the
// keyboard area (status bar) or below the last row.
func (r *Renderer) KeyAtWindow(kb *KeyboardState, px, py int32) (int, int, bool) {
	top := r.statusH
	if py < top {
		return 0, 0, false
	}
	ri := int(py-top) / int(r.unit+r.pad)
	if ri < 0 || ri >= len(kb.Layout) {
		return 0, 0, false
	}
	row := kb.Layout[ri]

	bestCol := 0
	bestDist := int64(1) << 62
	wpx, _ := keyRowPix(row, r.unitF, r.pad)
	x := int32(0)
	for ci := range row {
		center := x + wpx[ci]/2
		dist := int64(px) - int64(center)
		if dist < 0 {
			dist = -dist
		}
		if dist < bestDist {
			bestDist = dist
			bestCol = ci
		}
		x += wpx[ci] + r.pad
	}
	return ri, bestCol, true
}

// AccentCellAt reports the accent popup cell index under a window pixel
// point, or ok=false when the point is outside the popup's cells.
func (r *Renderer) AccentCellAt(kb *KeyboardState, px, py int32) (int, bool) {
	if kb.AccentPopup == nil {
		return 0, false
	}
	row := kb.Layout[kb.CursorRow]

	wpx, _ := keyRowPix(row, r.unitF, r.pad)
	xo := int32(0)
	for i := 0; i < kb.CursorCol; i++ {
		xo += wpx[i] + r.pad
	}
	ky := r.statusH + int32(kb.CursorRow)*(r.unit+r.pad) //nolint:gosec // G115: cursor index fits in int32
	py0 := ky - r.unit - r.pad
	cellTop := py0 + r.pad

	if py < cellTop || py >= cellTop+r.unit {
		return 0, false
	}
	for i := range kb.AccentPopup.Accents {
		cx := xo + r.pad + int32(i)*(r.unit+r.pad) //nolint:gosec // G115: accent index fits in int32
		if px >= cx && px < cx+r.unit {
			return i, true
		}
	}
	return 0, false
}

func (r *Renderer) SetTheme(t Theme) {
	r.theme = t
	r.flushTexCache()
	r.Flash(t.Name)
}

func (r *Renderer) flushTexCache() {
	for k, entry := range r.texCache {
		if entry.tex != nil {
			SDL3DestroyTexture(entry.tex)
		}
		delete(r.texCache, k)
	}
}

func (r *Renderer) Flash(text string) {
	r.flashText = text
	r.flashGlyphText = ""
	r.flashEnd = time.Now().Add(2 * time.Second)
	r.MarkDirty()
}

// FlashGlyph renders a Promptfont glyph followed by text in the normal font.
func (r *Renderer) FlashGlyph(glyph string, text string) {
	r.flashGlyphText = glyph
	r.flashText = text
	r.flashEnd = time.Now().Add(2 * time.Second)
	r.MarkDirty()
}

func (r *Renderer) Destroy() {
	r.flushTexCache()
	if r.font != nil {
		TTF3CloseFont(r.font)
	}
	if r.fontSmall != nil {
		TTF3CloseFont(r.fontSmall)
	}
	if r.fontGlyph != nil {
		TTF3CloseFont(r.fontGlyph)
	}
}

// keyRowPix converts a fractional unit into exact integer key widths for one
// row. Largest-remainder allocation splits the fractional pixels so the row's
// total spans exactly round(sum(width_k*unitF)) plus the inter-key gaps. For the
// widest row that total equals the screen width, so rendering and hit-testing
// agree pixel-for-pixel: a key is never clipped at the edge, never leaves a
// sliver. Both Draw and the *_At hit-test paths use this layout.
func keyRowPix(row []KeyDef, unitF float64, pad int32) ([]int32, int32) {
	n := len(row)
	px := make([]int32, n)
	if n == 0 {
		return px, 0
	}
	wf := make([]float64, n)
	total := 0.0
	for i, k := range row {
		v := k.Width * unitF
		wf[i] = v
		px[i] = int32(math.Floor(v))
		total += v
	}
	target := int32(math.Round(total + float64(n-1)*float64(pad)))
	rem := int(target-int32(n-1)*pad) - int32sum(px)
	if rem > 0 && rem <= n {
		idx := make([]int, n)
		for i := range idx {
			idx[i] = i
		}
		sort.SliceStable(idx, func(a, b int) bool {
			return wf[idx[a]]-float64(px[idx[a]]) > wf[idx[b]]-float64(px[idx[b]])
		})
		for i := 0; i < rem; i++ {
			px[idx[i]]++
		}
	}
	return px, target
}

func int32sum(a []int32) int {
	s := 0
	for _, v := range a {
		s += int(v)
	}
	return s
}

func CalcUnitSizeF(layout [][]KeyDef, screenWidth int32, cfg Config) float64 {
	if cfg.Keys.UnitSize > 0 {
		return float64(cfg.Keys.UnitSize)
	}
	scale := float64(cfg.Keys.Scale) / 100.0
	if scale <= 0 {
		scale = 0.70
	}
	target := float64(screenWidth) * scale
	pad := float64(cfg.Keys.Padding)
	// Solve unit per row so that row fits target exactly, then take the
	// SLOWEST unit (the widest row in pixels). Every other row is then
	// mathematically <= target, so the window can never overshoot the screen
	// (the old max-units-only estimate let a key-heavy row spill past it).
	unit := -1.0
	for _, row := range layout {
		ru := 0.0
		for _, k := range row {
			ru += k.Width
		}
		if ru <= 0 {
			continue
		}
		// Keys run flush to both screen edges (no outer padding); only the
		// gaps BETWEEN keys are padded. Round to nearest so the widest row
		// fills the target instead of leaving a sub-pixel runt each side.
		n := float64(len(row) - 1)
		if n < 0 {
			n = 0
		}
		rowUnit := (target - n*pad) / ru
		if unit < 0 || rowUnit < unit {
			unit = rowUnit
		}
	}
	if unit < 30 {
		unit = 30
	}
	return unit
}

func CalcWindowSize(layout [][]KeyDef, unitF float64, pad, statusH int32) (int32, int32) {
	var maxW int32
	for _, row := range layout {
		_, rw := keyRowPix(row, unitF, pad)
		if rw > maxW {
			maxW = rw
		}
	}
	n := int32(len(layout)) //nolint:gosec // G115: layout row count fits in int32
	rows := max32(0, n-1)
	u := int32(math.Round(unitF))
	h := statusH + n*u + rows*pad
	return maxW, h
}

func setColor(r *SDLRenderer, c Color) {
	SDL3SetRenderDrawColor(r, c)
}

func isModActive(key KeyDef, kb *KeyboardState) bool {
	switch key.ModifierType {
	case "shift":
		return kb.ShiftActive
	case "caps":
		return kb.CapsActive
	case "ctrl":
		return kb.CtrlActive
	case "alt":
		return kb.AltActive
	case "meta":
		return kb.MetaActive
	}
	return false
}

func findFont(names ...string) string {
	// Common font directories on Linux
	dirs := []string{
		"/usr/share/fonts/TTF",
		"/usr/share/fonts/truetype/dejavu",
		"/usr/share/fonts/truetype/liberation",
		"/usr/share/fonts/truetype/freefont",
		"/usr/share/fonts/truetype",
		"/usr/share/fonts",
	}
	filePatterns := map[string][]string{
		"DejaVu Sans":     {"DejaVuSans.ttf"},
		"Liberation Sans": {"LiberationSans-Regular.ttf"},
		"FreeSans":        {"FreeSans.ttf"},
	}

	for _, name := range names {
		patterns := filePatterns[name]
		for _, dir := range dirs {
			for _, pat := range patterns {
				path := dir + "/" + pat
				if _, err := os.Stat(path); err == nil {
					return path
				}
			}
		}
	}
	return ""
}

// hasPromptfontRune returns true if the string contains codepoints from
// Promptfont's mapped range (U+2600-U+27FF).
func hasPromptfontRune(s string) bool {
	for _, r := range s {
		if r >= 0x2600 && r <= 0x27FF {
			return true
		}
	}
	return false
}

func max32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}

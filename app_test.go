package main

import (
	"encoding/binary"
	"os"
	"testing"
)

const inputEventSize = 24

// newRecordingInjector returns an Injector whose events land in a temp file,
// so tests can count real key emissions without opening /dev/uinput.
func newRecordingInjector(t *testing.T) (*Injector, *os.File) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "events")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	return &Injector{fd: f}, f
}

// countKeyDown counts value=1 events for code in the recorded stream.
func countKeyDown(t *testing.T, f *os.File, code int) int {
	t.Helper()
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatalf("seek: %v", err)
	}
	buf := make([]byte, inputEventSize)
	n := 0
	for {
		if _, err := f.Read(buf); err != nil {
			break
		}
		if binary.LittleEndian.Uint16(buf[16:]) != evKey {
			continue
		}
		if int(binary.LittleEndian.Uint16(buf[18:])) == code &&
			int32(binary.LittleEndian.Uint32(buf[20:])) == 1 {
			n++
		}
	}
	return n
}

func findKey(t *testing.T, kb *KeyboardState, label string) (int, int) {
	t.Helper()
	for ri, row := range kb.Layout {
		for ci, kd := range row {
			if kd.Label == label {
				return ri, ci
			}
		}
	}
	t.Fatalf("key %q not found in layout", label)
	return 0, 0
}

func keyCenterPx(r *Renderer, layout [][]KeyDef, ri, ci int) (int32, int32) {
	w, _ := keyRowPix(layout[ri], r.unitF, r.pad)
	x := int32(0)
	for i := 0; i < ci; i++ {
		x += w[i] + r.pad
	}
	y := r.statusH + int32(ri)*(r.unit+r.pad) + r.unit/2
	return x + w[ci]/2, y
}

// TestRepeatableKeyClassification pins which keys repeat while held. The
// pointer and gamepad paths share this predicate, so a change here silently
// changes both.
func TestRepeatableKeyClassification(t *testing.T) {
	shouldRepeat := map[string]bool{
		"q": true, "a": true, "z": true, "5": true, "Space": true,
		"Esc": false, "Cfg": false, "Paste": false, "Shift": false,
		"Caps": false, "Ctrl": false, "Super": false, "Alt": false,
		"AltTab": false, "Home": false, "Del": true,
		"Bksp": true, "Enter": true,
	}
	for label, want := range shouldRepeat {
		var found bool
		for _, row := range LayoutQWERTY {
			for _, kd := range row {
				if kd.Label != label {
					continue
				}
				found = true
				if got := repeatableKey(kd); got != want {
					t.Errorf("repeatableKey(%q) = %v, want %v", label, got, want)
				}
			}
		}
		if !found {
			t.Errorf("key %q not present in LayoutQWERTY", label)
		}
	}
}

// TestPointerDownFiresOnceAndRepeats covers the new touch semantics: a
// repeatable key emits on pointer-DOWN (not release) and arms the hold
// repeat, and the matching pointer-UP must not emit it a second time.
func TestPointerDownFiresOnceAndRepeats(t *testing.T) {
	kb := NewKeyboardState(LayoutQWERTY)
	r := &Renderer{unit: 79, unitF: 79.2, pad: 4, statusH: 31}

	app := &App{visible: true}
	inj, rec := newRecordingInjector(t)

	row, col := findKey(t, kb, "q")
	x, y := keyCenterPx(r, kb.Layout, row, col)

	app.handlePointer(pointerDown, x, y, kb, inj, r)

	if got := countKeyDown(t, rec, KEY_Q); got != 1 {
		t.Errorf("after pointerDown, KEY_Q emitted %d times, want 1", got)
	}
	if app.repeatAction != ActionPressRepeat {
		t.Errorf("repeatAction = %v, want ActionPressRepeat (hold should repeat)", app.repeatAction)
	}
	if !app.pointerFired {
		t.Error("pointerFired = false, want true after firing on press")
	}

	app.handlePointer(pointerUp, x, y, kb, inj, r)

	if got := countKeyDown(t, rec, KEY_Q); got != 1 {
		t.Errorf("after pointerUp, KEY_Q emitted %d times, want 1 (no double fire)", got)
	}
	if app.repeatAction != ActionNone {
		t.Errorf("repeatAction = %v, want ActionNone after release", app.repeatAction)
	}
	if app.pointerFired {
		t.Error("pointerFired = true, want false after release")
	}
}

// A non-repeatable key must still fire on release, and must not arm repeat.
func TestPointerNonRepeatableFiresOnPress(t *testing.T) {
	kb := NewKeyboardState(LayoutQWERTY)
	r := &Renderer{unit: 79, unitF: 79.2, pad: 4, statusH: 31}
	app := &App{visible: true}
	inj, rec := newRecordingInjector(t)

	row, col := findKey(t, kb, "Esc")
	x, y := keyCenterPx(r, kb.Layout, row, col)

	app.handlePointer(pointerDown, x, y, kb, inj, r)
	if got := countKeyDown(t, rec, KEY_ESC); got != 1 {
		t.Errorf("after pointerDown, KEY_ESC emitted %d times, want 1", got)
	}
	if app.repeatAction != ActionNone {
		t.Errorf("repeatAction = %v, want ActionNone for Esc", app.repeatAction)
	}

	app.handlePointer(pointerUp, x, y, kb, inj, r)
	if got := countKeyDown(t, rec, KEY_ESC); got != 1 {
		t.Errorf("after pointerUp, KEY_ESC emitted %d times, want 1 (no double fire)", got)
	}
}

// Modifiers fire on press too, and stay engaged after release (sticky toggle).
func TestPointerModifierFiresOnPress(t *testing.T) {
	kb := NewKeyboardState(LayoutQWERTY)
	r := &Renderer{unit: 79, unitF: 79.2, pad: 4, statusH: 31}
	app := &App{visible: true}
	inj, rec := newRecordingInjector(t)

	row, col := findKey(t, kb, "Shift")
	x, y := keyCenterPx(r, kb.Layout, row, col)

	if kb.ShiftActive {
		t.Fatal("precondition: shift should start inactive")
	}
	app.handlePointer(pointerDown, x, y, kb, inj, r)
	if !kb.ShiftActive {
		t.Error("ShiftActive = false after pointerDown, want true (fire on press)")
	}
	if app.repeatAction != ActionNone {
		t.Errorf("repeatAction = %v, want ActionNone for a modifier", app.repeatAction)
	}

	app.handlePointer(pointerUp, x, y, kb, inj, r)
	if !kb.ShiftActive {
		t.Error("ShiftActive = false after pointerUp, want it to stay engaged")
	}
	if got := countKeyDown(t, rec, KEY_LEFTSHIFT); got != 0 {
		t.Errorf("KEY_LEFTSHIFT emitted %d times, want 0 (sticky toggle, not injected)", got)
	}
}

// A tap landing off the grid (status bar, below the last row) must not fire
// the key the cursor happens to be resting on.
func TestPointerOffGridDoesNotFire(t *testing.T) {
	kb := NewKeyboardState(LayoutQWERTY)
	r := &Renderer{unit: 79, unitF: 79.2, pad: 4, statusH: 31}
	app := &App{visible: true}
	inj, rec := newRecordingInjector(t)

	row, col := findKey(t, kb, "q")
	x, _ := keyCenterPx(r, kb.Layout, row, col)
	kb.CursorRow, kb.CursorCol = row, col // cursor resting on q

	above := r.statusH - 1 // inside the status bar, above the first row
	below := r.statusH + int32(len(kb.Layout))*(r.unit+r.pad) + r.unit/2
	for _, py := range []int32{above, below} {
		app.handlePointer(pointerDown, x, py, kb, inj, r)
	}

	if got := countKeyDown(t, rec, KEY_Q); got != 0 {
		t.Errorf("off-grid taps emitted KEY_Q %d times, want 0", got)
	}
	if app.pointerFired {
		t.Error("pointerFired = true after an off-grid tap, want false")
	}
	if app.repeatAction != ActionNone {
		t.Errorf("repeatAction = %v, want ActionNone after off-grid taps", app.repeatAction)
	}
}

// Sliding off the key that took the press must stop the repeat rather than
// repeating whatever the finger now covers.
func TestPointerSlideOffStopsRepeat(t *testing.T) {
	kb := NewKeyboardState(LayoutQWERTY)
	r := &Renderer{unit: 79, unitF: 79.2, pad: 4, statusH: 31}

	app := &App{visible: true}
	inj, _ := newRecordingInjector(t)

	row, col := findKey(t, kb, "q")
	x, y := keyCenterPx(r, kb.Layout, row, col)
	app.handlePointer(pointerDown, x, y, kb, inj, r)
	if app.repeatAction != ActionPressRepeat {
		t.Fatalf("precondition: repeat not armed")
	}

	otherRow, otherCol := findKey(t, kb, "p")
	ox, oy := keyCenterPx(r, kb.Layout, otherRow, otherCol)
	app.handlePointer(pointerMove, ox, oy, kb, inj, r)

	if app.repeatAction != ActionNone {
		t.Errorf("repeatAction = %v, want ActionNone after sliding off", app.repeatAction)
	}
}

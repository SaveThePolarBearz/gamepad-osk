package main

import "testing"

func TestKeyAtWindow(t *testing.T) {
	r := &Renderer{unit: 50, unitF: 50, pad: 4, statusH: 20}
	kb := NewKeyboardState(LayoutQWERTY)
	top := r.statusH

	for ri, row := range kb.Layout {
		y := top + int32(ri)*(r.unit+r.pad) + r.unit/2
		x := int32(0)
		for ci, key := range row {
			w := int32(key.Width * float64(r.unit)) // unit is integral in tests: == keyRowPix
			center := x + w/2
			gotR, gotC, ok := r.KeyAtWindow(kb, center, y)
			if !ok || gotR != ri || gotC != ci {
				t.Errorf("KeyAtWindow(center of [%d][%d] %q) = (%d,%d,%v), want (%d,%d,true)",
					ri, ci, key.Label, gotR, gotC, ok, ri, ci)
			}
			x += w + r.pad
		}
	}

	if _, _, ok := r.KeyAtWindow(kb, r.pad, top-1); ok {
		t.Error("point above keyboard rows should be ok=false")
	}
	below := top + int32(len(kb.Layout))*(r.unit+r.pad)
	if _, _, ok := r.KeyAtWindow(kb, r.pad, below); ok {
		t.Error("point below keyboard should be ok=false")
	}
}

// Every pixel of every key's drawn rect must resolve back to that key. A
// nearest-centre hit test lets the right portion of the space bar resolve to
// the left arrow, because the space bar's centre sits further from that point
// than the arrow's centre does. Uses a fractional unit so keyRowPix's
// largest-remainder allocation is exercised, as on the real device.
func TestKeyAtWindowCoversWholeKeyRect(t *testing.T) {
	r := &Renderer{unit: 79, unitF: 79.2, pad: 4, statusH: 31}
	kb := NewKeyboardState(LayoutQWERTY)
	top := r.statusH

	for ri, row := range kb.Layout {
		y := top + int32(ri)*(r.unit+r.pad) + r.unit/2
		wpx, _ := keyRowPix(row, r.unitF, r.pad)
		x := int32(0)
		for ci, key := range row {
			for px := x; px < x+wpx[ci]; px++ {
				gotR, gotC, ok := r.KeyAtWindow(kb, px, y)
				if !ok || gotR != ri || gotC != ci {
					t.Fatalf("KeyAtWindow(x=%d) = (%d,%d,%v), want (%d,%d,true): x=%d is inside [%d][%d] %q",
						px, gotR, gotC, ok, ri, ci, px, ri, ci, key.Label)
				}
			}
			x += wpx[ci] + r.pad
		}
	}
}

func TestAccentCellAt(t *testing.T) {
	r := &Renderer{unit: 50, unitF: 50, pad: 4, statusH: 20}
	kb := NewKeyboardState(LayoutQWERTY)
	kb.CursorRow = 2
	kb.CursorCol = 1
	kb.AccentPopup = &AccentPopupState{Accents: accentE, Selected: 0}

	row := kb.Layout[kb.CursorRow]
	xo := int32(0)
	for i := 0; i < kb.CursorCol; i++ {
		xo += int32(row[i].Width*float64(r.unit)) + r.pad
	}
	ky := r.statusH + int32(kb.CursorRow)*(r.unit+r.pad)
	py0 := ky - r.unit - r.pad

	for i := range kb.AccentPopup.Accents {
		cx := xo + r.pad + int32(i)*(r.unit+r.pad)
		got, ok := r.AccentCellAt(kb, cx+r.unit/2, py0+r.pad+r.unit/2)
		if !ok || got != i {
			t.Errorf("AccentCellAt(center of cell %d) = (%d,%v), want (%d,true)", i, got, ok, i)
		}
	}

	if _, ok := r.AccentCellAt(kb, xo+r.pad+r.unit/2, py0); ok {
		t.Error("point above popup cells should be ok=false")
	}
}

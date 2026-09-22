package main //nolint:revive // main package needs no doc comment

import (
	"log"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var themeOrder []string
var isWayland bool

func init() {
	for name := range Themes {
		themeOrder = append(themeOrder, name)
	}
	sort.Strings(themeOrder)
}

type App struct {
	config            Config
	running           bool
	visible           bool
	daemon            bool
	touchOnly         bool
	togglePending     bool
	grabModePending   bool
	showPending       bool
	hidePending       bool
	outputPendingSet  bool
	outputPendingName string
	lock              sync.Mutex
	themeIdx          int
	posTop            bool // true = keyboard at top of screen
	window            *Window
	mon               MonitorRect // raw monitor bounds
	posArea           MonitorRect // positioning area (workarea on X11, same as mon on Wayland)
	winW              int32
	winH              int32
	winX              int32 // stored for re-apply after show
	winY              int32
	margin            int32

	// Key repeat state
	repeatAction  ActionType // action to repeat (ActionNone = inactive)
	repeatStart   time.Time  // when key was first pressed
	repeatLast    time.Time  // when last repeat fired
	repeatInitial bool       // true = still in initial delay phase

	reconnectLast time.Time // cooldown for gamepad reconnection attempts

	// Deferred grab: when the OSK becomes visible we wait for any held
	// trigger buttons to release before calling EVIOCGRAB, so other readers
	// of the device (evsieve hooks, gamepad mappers) see the release events
	// and don't end up with stuck per-button state. Bounded by a 500 ms
	// fallback so a user who never releases the combo still gets a grab.
	grabPending      bool
	grabPendingUntil time.Time

	// Pointer (touch/mouse) interaction: pointOnPopup tracks whether the
	// current pointer-down landed on the accent popup, so that release
	// confirms the highlighted accent rather than a keyboard key.
	pointOnPopup bool
}

func NewApp(config Config) *App {
	return &App{
		config:  config,
		visible: true,
	}
}

func (app *App) Run() error {
	// SDL/OpenGL requires all calls from the same OS thread.
	// go-sdl2 did this internally in sdl.Init(); we must do it explicitly.
	runtime.LockOSThread()

	cfg := app.config
	ValidateConfig(&cfg)
	layout := LayoutQWERTY
	theme := GetTheme(cfg.Theme.Name)

	// Build dynamic glyph map from button config
	KeyGlyphs = BuildKeyGlyphs(cfg.Gamepad)

	// Prefer native Wayland over XWayland when running in a Wayland session
	if os.Getenv("WAYLAND_DISPLAY") != "" && os.Getenv("SDL_VIDEODRIVER") == "" {
		if err := os.Setenv("SDL_VIDEODRIVER", "wayland"); err != nil {
			log.Printf("Warning: cannot set SDL_VIDEODRIVER: %v", err)
		}
	}

	// Allow the screensaver and sleep timers to fire. SDL_Init's video
	// subsystem disables the screensaver by default; on X11 that schedules
	// XResetScreenSaver every ~30s, which resets the idle counter and blocks
	// screen blank, DPMS, and suspend. The OSK has no use for an always-on
	// display, so opt out before init. On Wayland this avoids requesting
	// idle-inhibit.
	SDL3SetHint("SDL_VIDEO_ALLOW_SCREENSAVER", "1")

	// Touch events must drive the keyboard directly; don't let SDL also turn
	// them into synthetic mouse clicks or every tap would press twice.
	SDL3SetHint("SDL_TOUCH_MOUSE_EVENTS", "0")

	if err := SDL3Init(SDL_INIT_VIDEO); err != nil {
		return err
	}
	defer SDL3Quit()

	initX11Detection()

	isWayland = os.Getenv("WAYLAND_DISPLAY") != ""

	// Get primary monitor
	mon := GetPrimaryMonitor()
	// Wayland: the layer surface lands on a specific wl_output. Size from THAT
	// output's real pixel width (config `output = <name>`, else the SDL primary
	// matched by size), so scale=100 spans the full screen it shows on. Falls
	// back to SDL primary bounds on X11 or if unmatched.
	if isWayland {
		if cfg.Window.Output != "" {
			if tw, th, ok := getNamedOutputInfo(cfg.Window.Output); ok && tw > 0 {
				Debugf("Target output %q is %dx%d (logical)", cfg.Window.Output, tw, th)
				mon.W, mon.H = tw, th
			} else {
				Debugf("Configured output %q not found, sizing from SDL primary width %d", cfg.Window.Output, mon.W)
			}
		} else if tw, th, ok := getTargetOutputInfo(mon.W, mon.H); ok && tw > 0 {
			Debugf("Target output is %dx%d (SDL primary said %dx%d)", tw, th, mon.W, mon.H)
			mon.W, mon.H = tw, th
		} else {
			Debugf("No wl_output match for %dx%d, sizing from SDL primary width %d", mon.W, mon.H, mon.W)
		}
	}
	unit := CalcUnitSizeF(layout, mon.W, cfg)
	pad := int32(cfg.Keys.Padding) //nolint:gosec // G115: padding fits in int32
	statusH := max32(20, int32(unit*0.4))
	width, height := CalcWindowSize(layout, unit, pad, statusH)
	Debugf("Monitor: %dx%d+%d+%d, scale=%d%%, unit=%d, window=%dx%d",
		mon.W, mon.H, mon.X, mon.Y, cfg.Keys.Scale, int32(unit), width, height)

	// Use workarea on X11 to avoid panels/taskbars; fall back to monitor bounds.
	// Workarea spans all monitors - intersect with primary to get per-monitor area.
	posArea := mon
	waOk := false
	if wx, wy, ww, wh, ok := GetWorkarea(); ok && ww > 0 && wh > 0 {
		waOk = true
		wa := MonitorRect{X: wx, Y: wy, W: ww, H: wh}
		posArea = intersectRect(mon, wa)
		Debugf("Workarea: %dx%d+%d+%d, monitor: %dx%d+%d+%d, effective: %dx%d+%d+%d",
			ww, wh, wx, wy, mon.W, mon.H, mon.X, mon.Y,
			posArea.W, posArea.H, posArea.X, posArea.Y)
	}
	// Fallback: workarea available but didn't subtract panel for this monitor
	// (multi-monitor XFCE reports combined workarea without per-monitor panel gaps).
	// Scan _NET_WM_STRUT_PARTIAL on dock/panel windows directly.
	if waOk && posArea == mon {
		if top, bottom, left, right := GetStrutInsets(mon); top > 0 || bottom > 0 || left > 0 || right > 0 {
			posArea = MonitorRect{
				X: mon.X + left,
				Y: mon.Y + top,
				W: mon.W - left - right,
				H: mon.H - top - bottom,
			}
			Debugf("Strut fallback: top=%d bottom=%d left=%d right=%d, effective: %dx%d+%d+%d",
				top, bottom, left, right, posArea.W, posArea.H, posArea.X, posArea.Y)
		}
	}

	// Position: center horizontally, top or bottom based on config
	x := posArea.X + (posArea.W-width)/2
	margin := int32(cfg.Window.Margin) //nolint:gosec // G115: margin fits in int32

	SaveFocusedWindow()

	// Set Wayland app_id so compositor rules can target this window
	SDL3SetHint("SDL_APP_ID", "gamepad-osk")
	if err := os.Setenv("SDL_APP_ID", "gamepad-osk"); err != nil {
		log.Printf("Warning: cannot set SDL_APP_ID: %v", err)
	}

	var window *Window
	var err error
	if isWayland {
		// Wayland: create roleless surface, attach layer-shell asynchronously.
		// Target the configured wl_output by name, else the SDL primary by size.
		if cfg.Window.Output != "" {
			setTargetName(cfg.Window.Output)
		} else {
			setTargetSize(mon.W, mon.H)
		}
		window, err = createWaylandWindow("gamepad-osk", width, height)
		if err != nil {
			return err
		}
	} else {
		// X11: override-redirect handles z-ordering; SDL_WINDOW_ALWAYS_ON_TOP omitted.
		// SDL3's X11 backend sends _NET_WM_STATE_ABOVE via XSendEvent to root when
		// showing a window with that flag. xfwm4 processes it (even for override-redirect
		// windows) and triggers fullscreen stack re-evaluation, causing xfce4-panel to
		// re-appear. Override-redirect + XMapRaised (called by SDL ShowWindow) provides
		// z-ordering without notifying the WM.
		window, err = SDL3CreateWindow("gamepad-osk",
			width, height,
			SDL_WINDOW_HIDDEN|SDL_WINDOW_BORDERLESS)
		if err != nil {
			return err
		}
		// Position set after app fields are initialized (computeY needs them)
	}
	defer func() {
		cleanupLayerShell()
		SDL3DestroyWindow(window)
	}()

	// Store for position toggling (must be set before computeY)
	app.window = window
	app.mon = mon
	app.posArea = posArea
	app.winW = width
	app.winH = height
	app.winX = x
	app.margin = margin
	app.posTop = cfg.Window.Position == "top"
	app.winY = app.computeY()

	if !isWayland {
		SDL3SetWindowPosition(window, x, app.winY)
	}

	// Attach layer-shell asynchronously -- configure arrives via SDL event pump.
	// Skip in daemon mode (starts hidden, first toggle-show will attach).
	if isWayland && app.visible {
		attachLayerShellAsync(window, width, height, app.posTop, margin, cfg.Window.Mode)
	}

	renderer, err := SDL3CreateRenderer(window)
	if err != nil {
		return err
	}
	defer SDL3DestroyRenderer(renderer)
	SDL3SetRenderVSync(renderer, 1)
	SDL3SetRenderDrawBlendMode(renderer, 1) // SDL_BLENDMODE_BLEND
	Debugf("Renderer: %s", SDL3GetRendererName(renderer))

	// Set opacity after renderer creation (SDL3 requires renderer to exist first)
	if cfg.Window.Opacity < 1.0 {
		SDL3SetWindowOpacity(window, float32(cfg.Window.Opacity))
	}

	// Set X11 hints AFTER renderer init (layer-shell handles this on Wayland)
	if !hasLayerShell() {
		SetNoFocusHints(window)
		// No RestoreFocus here: window is hidden, SDL init does not steal X11 focus when a WM is present
	}

	rend, err := NewRenderer(renderer, theme, unit, pad)
	if err != nil {
		return err
	}
	defer rend.Destroy()

	inj, err := NewInjector()
	if err != nil {
		log.Print(colorRed("Warning: key injection disabled - " + err.Error()))
		logPermissionFix()
		log.Printf("The on-screen keyboard will display but cannot send keystrokes.")
	}
	defer func() {
		if inj != nil {
			inj.Close()
		}
	}()

	gamepad := NewGamepadReader(cfg)
	if !app.touchOnly && !gamepad.Open("") {
		log.Printf("Warning: no gamepad found")
		log.Printf("Usage: gamepad-osk --device /dev/input/gamepad0")
	}
	if app.touchOnly {
		log.Printf("Touch-only mode: gamepad reader disabled (no open, no grab)")
	}
	defer gamepad.Close()
	if !app.touchOnly && cfg.Gamepad.Grab && app.visible {
		app.grabPending = true
		app.grabPendingUntil = time.Now().Add(500 * time.Millisecond)
	}

	// Rebuild glyphs after gamepad open (swap_xy may have been auto-detected)
	KeyGlyphs = BuildKeyGlyphs(gamepad.config.Gamepad)

	// Init IPC
	ipc := NewIPCServer(func(cmd string) {
		if strings.HasPrefix(cmd, "output ") {
			name := strings.TrimSpace(strings.TrimPrefix(cmd, "output"))
			app.lock.Lock()
			app.outputPendingName = name
			app.outputPendingSet = true
			app.lock.Unlock()
			return
		}
		switch cmd {
		case "toggle":
			app.lock.Lock()
			app.togglePending = true
			app.lock.Unlock()
		case "show":
			app.lock.Lock()
			app.showPending = true
			app.lock.Unlock()
		case "hide":
			app.lock.Lock()
			app.hidePending = true
			app.lock.Unlock()
		case "ping":
			// Instance check - connection success is the answer
		case "grab-mode-on":
			app.lock.Lock()
			app.touchOnly = false
			app.grabModePending = true
			app.lock.Unlock()
		case "grab-mode-off":
			app.lock.Lock()
			app.touchOnly = true
			app.grabModePending = true
			app.lock.Unlock()
		}
	})
	if err := ipc.Start(); err != nil {
		log.Printf("Warning: IPC server failed: %v", err)
	}
	defer ipc.Stop()

	kb := NewKeyboardState(layout)

	// Find current theme index for cycling
	for i, name := range themeOrder {
		if name == cfg.Theme.Name {
			app.themeIdx = i
			break
		}
	}
	kb.OnThemeCycle = func() {
		app.themeIdx = (app.themeIdx + 1) % len(themeOrder)
		name := themeOrder[app.themeIdx]
		rend.SetTheme(Themes[name])
		SaveTheme(name)
	}
	kb.OnThemeCycleReverse = func() {
		app.themeIdx = (app.themeIdx - 1 + len(themeOrder)) % len(themeOrder)
		name := themeOrder[app.themeIdx]
		rend.SetTheme(Themes[name])
		SaveTheme(name)
	}
	kb.OnSensitivityUp = func() {
		cfg.Mouse.Sensitivity = min(50, cfg.Mouse.Sensitivity+2)
		gamepad.SetSensitivity(cfg.Mouse.Sensitivity)
		rend.FlashGlyph("\u27F9", strconv.Itoa(cfg.Mouse.Sensitivity))
		SaveSensitivity(cfg.Mouse.Sensitivity)
	}
	kb.OnSensitivityDown = func() {
		cfg.Mouse.Sensitivity = max(1, cfg.Mouse.Sensitivity-2)
		gamepad.SetSensitivity(cfg.Mouse.Sensitivity)
		rend.FlashGlyph("\u27FE", strconv.Itoa(cfg.Mouse.Sensitivity))
		SaveSensitivity(cfg.Mouse.Sensitivity)
	}

	app.running = true

	opacity := float32(cfg.Window.Opacity)

	// Show window after everything is initialized (avoids ghost frame)
	if app.visible {
		if !isWayland {
			rend.Draw(kb) // render first frame before showing (Wayland: gated by IsLayerShellReady)
			SDL3ShowWindow(window)
			if opacity < 1.0 {
				SDL3SetWindowOpacity(window, opacity) // re-apply after show
			}
			SDL3SetWindowPosition(window, x, app.winY)
			SetNoFocusHints(window)
		}
		// Wayland: first frame renders after configure arrives via SDL event pump
	}

	for app.running {
		// Handle toggle / explicit show / explicit hide
		app.lock.Lock()
		app.applyShowHide(inj, kb, rend, gamepad, cfg.Gamepad.Grab, hasLayerShell(), isWayland, window, width, height, app.posTop, margin, cfg.Window.Mode, opacity)
		app.lock.Unlock()

		// Live output switch (IPC `output <name>`): re-target the layer surface
		// without restarting. Recomputes the window size for the new target's
		// width, resizes, and re-attaches when visible. Empty name = SDL primary.
		if app.outputPendingSet {
			app.outputPendingSet = false
			targetOk := false
			if app.outputPendingName != "" {
				if tw, th2, ok := getNamedOutputInfo(app.outputPendingName); ok && tw > 0 {
					Debugf("Switching to output %q (%dx%d logical)", app.outputPendingName, tw, th2)
					setTargetName(app.outputPendingName)
					cfg.Window.Output = app.outputPendingName
					mon.W = tw
					mon.H = th2
					targetOk = true
				} else {
					log.Printf("Warning: output %q not found, keeping current target", app.outputPendingName)
				}
			} else {
				// Empty name: back to the SDL primary, re-fetched at switch time
				// (it may have changed while running).
				mon = GetPrimaryMonitor()
				setTargetSize(mon.W, mon.H)
				cfg.Window.Output = ""
				if tw, th2, ok := getTargetOutputInfo(mon.W, mon.H); ok && tw > 0 {
					mon.W, mon.H = tw, th2
				}
				Debugf("Switching back to SDL primary (%dpx wide)", mon.W)
				targetOk = true
			}
			if targetOk {
				unit = CalcUnitSizeF(layout, mon.W, cfg)
				pad = int32(cfg.Keys.Padding) //nolint:gosec // G115: padding fits in int32
				statusH = max32(20, int32(unit*0.4))
				width, height = CalcWindowSize(layout, unit, pad, statusH)
				x = app.posArea.X + (app.posArea.W-width)/2
				app.winW, app.winH = width, height
				app.winX = x
				app.mon = mon
				Debugf("Output switch complete: window=%dx%d on %q (unit=%d)", width, height, cfg.Window.Output, int32(unit))
				if isWayland {
					SDL3SetWindowSize(window, width, height)
					if app.visible {
						// Re-attach so the surface moves to the new target output.
						destroyLayerSurface()
						attachLayerShellAsync(window, width, height, app.posTop, margin, cfg.Window.Mode)
					}
				}
				rend.SetScale(unit, pad, statusH)
			}
		}

		// Live grab-mode switch: re-open or close the gamepad reader without
		// restarting the daemon. Runs on the render thread; IPC handler only
		// set the pending flag. Idempotent: keyed off actual fd state.
		if app.grabModePending {
			app.grabModePending = false
			if app.touchOnly {
				if gamepad.Fd() >= 0 {
					app.grabPending = false
					gamepad.Ungrab()
					gamepad.Close()
					KeyGlyphs = BuildKeyGlyphs(gamepad.config.Gamepad)
					log.Printf("Grab mode off: gamepad reader closed (touch only)")
				}
			} else {
				if gamepad.Fd() < 0 {
					if gamepad.Open("") {
						KeyGlyphs = BuildKeyGlyphs(gamepad.config.Gamepad)
						if app.visible && cfg.Gamepad.Grab {
							app.grabPending = true
							app.grabPendingUntil = time.Now().Add(500 * time.Millisecond)
						}
						log.Printf("Grab mode on: gamepad reader active")
					} else if cfg.Gamepad.Device == "" {
						log.Printf("Grab mode on: no gamepad found (recheck on reconnect)")
					}
				}
			}
		}

		// Process SDL3 window / touch / mouse events
		for ev, ok := SDL3PollEventData(); ok; ev, ok = SDL3PollEventData() {
			switch ev.Type {
			case SDL_EVENT_QUIT:
				app.running = false
			case SDL_EVENT_FINGER_DOWN:
				px, py := app.touchToPx(ev)
				app.handlePointer(pointerDown, px, py, kb, inj, rend)
			case SDL_EVENT_FINGER_MOTION:
				px, py := app.touchToPx(ev)
				app.handlePointer(pointerMove, px, py, kb, inj, rend)
			case SDL_EVENT_FINGER_UP, SDL_EVENT_FINGER_CANCELED:
				px, py := app.touchToPx(ev)
				app.handlePointer(pointerUp, px, py, kb, inj, rend)
			case SDL_EVENT_MOUSE_BUTTON_DOWN:
				if ev.MouseButton == 1 {
					app.handlePointer(pointerDown, int32(ev.MouseX), int32(ev.MouseY), kb, inj, rend)
				}
			case SDL_EVENT_MOUSE_BUTTON_UP:
				if ev.MouseButton == 1 {
					app.handlePointer(pointerUp, int32(ev.MouseX), int32(ev.MouseY), kb, inj, rend)
				}
			}
		}

		// Process gamepad events (evdev - works regardless of window focus)
		prevFd := gamepad.Fd()
		for _, action := range gamepad.ReadEvents() {
			app.handleAction(action, kb, inj, rend)
		}
		if prevFd >= 0 && gamepad.Fd() < 0 {
			rend.Flash("Controller lost")
		}

		// Deferred grab: defer EVIOCGRAB until trigger combo is released so
		// other readers (evsieve hooks, gamepad mappers) see the release events
		// and don't end up with stuck per-button state. Mirrors evsieve's
		// own grab=auto idiom. Falls back to grabbing after grabPendingUntil
		// so a user holding the combo indefinitely still gets a grab.
		if app.grabPending {
			if !gamepad.AnyButtonHeld() || time.Now().After(app.grabPendingUntil) {
				app.grabPending = false
				gamepad.Grab()
			}
		}

		// Gamepad reconnection (2s cooldown between attempts)
		if !app.touchOnly && gamepad.Fd() < 0 {
			now := time.Now()
			if now.Sub(app.reconnectLast) >= 2*time.Second {
				app.reconnectLast = now
				if gamepad.Reconnect() {
					rend.Flash("Controller connected")
					if app.visible && cfg.Gamepad.Grab {
						gamepad.Grab()
					}
				}
			}
		}

		// Long-press check
		if kb.CheckLongPress(cfg.Gamepad.LongPressMs) {
			rend.MarkDirty()
		}

		// Alt+Tab timeout (auto-release after 3s idle)
		if kb.CheckAltTabTimeout(inj) {
			rend.MarkDirty()
		}

		// Key repeat check
		if app.repeatAction != ActionNone {
			now := time.Now()
			elapsed := now.Sub(app.repeatStart).Milliseconds()
			if app.repeatInitial {
				if elapsed >= int64(cfg.Keys.RepeatDelayMs) {
					app.handleAction(Action{Type: app.repeatAction}, kb, inj, rend)
					app.repeatLast = now
					app.repeatInitial = false
				}
			} else if now.Sub(app.repeatLast).Milliseconds() >= int64(cfg.Keys.RepeatRateMs) {
				app.handleAction(Action{Type: app.repeatAction}, kb, inj, rend)
				app.repeatLast = now
			}
		}

		// Check flash/key flash expiry to trigger final redraw
		if app.visible {
			if rend.flashText != "" && time.Now().After(rend.flashEnd) {
				rend.MarkDirty()
				rend.flashText = ""
				rend.flashGlyphText = ""
			}
			if kb.FlashCode != 0 && time.Now().After(kb.FlashUntil) {
				rend.MarkDirty()
				kb.FlashCode = 0
			}
		}

		// Render
		if app.visible {
			rend.Draw(kb)
		}

		// Frame pacing: vsync in rend.Draw handles active rendering.
		// Sleep when hidden, when visible but idle (nothing to draw), or
		// when stick is active (sub-vsync polling for smooth cursor).
		if !app.visible || rend.dirtyFrames <= 0 || gamepad.NeedsPolling() {
			pollMs := 16 // hidden idle: ~60Hz for IPC + gamepad checks
			if gamepad.NeedsPolling() {
				pollMs = 4 // mouse/nav active: ~250Hz for smooth cursor
			}
			if app.repeatAction != ActionNone {
				var nextMs int64
				if app.repeatInitial {
					nextMs = int64(cfg.Keys.RepeatDelayMs) - time.Since(app.repeatStart).Milliseconds()
				} else {
					nextMs = int64(cfg.Keys.RepeatRateMs) - time.Since(app.repeatLast).Milliseconds()
				}
				if nextMs > 0 && int(nextMs) < pollMs {
					pollMs = int(nextMs)
				}
			}
			if pollMs > 0 {
				time.Sleep(time.Duration(pollMs) * time.Millisecond)
			}
		}
	}
	return nil
}

// applyShowHide reconciles app.visible against a pending show/hide request
// (toggle flips; an explicit show/hide is idempotent). Callers must hold app.lock.
// It does nothing when no action is requested or the desired state already matches.
func (app *App) applyShowHide(inj *Injector, kb *KeyboardState, rend *Renderer, gamepad *GamepadReader, grab, hasLS, isWayland bool, window *Window, width, height int32, posTop bool, margin int32, mode string, opacity float32) {
	var wantShow bool
	switch {
	case app.showPending:
		wantShow = true
	case app.hidePending:
		wantShow = false
	case app.togglePending:
		wantShow = !app.visible
	default:
		return
	}
	app.togglePending = false
	app.showPending = false
	app.hidePending = false
	if app.visible == wantShow {
		return // already in the requested state (idempotent show/hide)
	}
	app.visible = wantShow

	if app.visible {
		if grab {
			app.grabPending = true
			app.grabPendingUntil = time.Now().Add(500 * time.Millisecond)
		}
		if !hasLS {
			SaveFocusedWindow() // capture game window now, used by RestoreFocus on hide
		}
		if isWayland {
			attachLayerShellAsync(window, width, height, posTop, margin, mode)
		} else {
			SDL3ShowWindow(window)
		}
		rend.MarkDirty()
		if opacity < 1.0 {
			SDL3SetWindowOpacity(window, opacity) // re-apply after show
		}
		if !isWayland {
			app.winY = app.computeY()
			SDL3SetWindowPosition(window, app.winX, app.winY)
			SetNoFocusHints(window)
			// No RestoreFocus: XMapRaised does not steal focus; calling XSetInputFocus here
			// changes _NET_ACTIVE_WINDOW to prev_focused (terminal), triggering xfce4-panel.
		}
	} else {
		app.stopRepeat()
		kb.CapsActive = false
		kb.ShiftActive = false
		kb.ShiftHeld = false
		kb.CtrlActive = false
		kb.AltActive = false
		kb.MetaActive = false
		kb.ReleaseAltTab(inj)
		if inj != nil {
			inj.ReleaseKey(KEY_LEFTSHIFT)
			inj.ReleaseKey(KEY_LEFTCTRL)
			inj.ReleaseKey(KEY_LEFTALT)
			inj.ReleaseKey(KEY_LEFTMETA)
			inj.ClickMouse(0x110, false) // BTN_LEFT release
			inj.ClickMouse(0x111, false) // BTN_RIGHT release
		}
		// Cancel any pending grab so it can't fire after hide.
		app.grabPending = false
		gamepad.Ungrab()
		if isWayland {
			destroyLayerSurface()
		} else {
			SDL3HideWindow(window)
		}
		if !isWayland {
			if IsSavedWindowFullscreen() {
				WarpPointerIfOutside()
			}
			RestoreFocus()
		}
	}
}

func (app *App) handleAction(a Action, kb *KeyboardState, inj *Injector, rend *Renderer) {
	// Toggle combo works even when hidden (it's the show/hide mechanism)
	if a.Type == ActionToggle {
		app.lock.Lock()
		app.togglePending = true
		app.lock.Unlock()
		return
	}

	// Block all input when keyboard is hidden
	if !app.visible {
		return
	}

	rend.MarkDirty()

	switch a.Type { //nolint:exhaustive // ActionNone is filtered by caller

	case ActionNavigate:
		kb.ReleaseAltTab(inj)
		kb.Navigate(a.DX, a.DY)
		app.stopRepeat()
	case ActionPress:
		// A button released
		if app.repeatAction != ActionNone {
			// Was repeating - just stop, don't fire again
			app.stopRepeat()
		} else {
			// Accent popup select, vowel short-press, or modifier - fire on release
			kb.PressCurrent(inj)
		}
		kb.CancelLongPress()
	case ActionPressStart:
		key := kb.CurrentKey()
		if len(key.Accents) > 0 && kb.ShiftActive {
			// Shift(LT)+hold on vowel - accent popup
			kb.StartLongPress()
		} else if !key.IsModifier && key.Label != "Cfg" && key.Label != "Paste" &&
			len(key.Combo) == 0 && key.ShiftCode == 0 && key.Label != "Esc" {
			// Repeatable key - fire immediately and start repeat
			kb.PressCurrent(inj)
			app.startRepeat(ActionPressRepeat)
		}
		// Non-repeatable keys (shortcuts, Esc, Cfg, Paste, modifiers) fire on release via ActionPress
	case ActionPressRepeat:
		kb.PressCurrent(inj)
	case ActionBackspace:
		if inj != nil {
			inj.PressKey(KEY_BACKSPACE, nil)
		}
		kb.FlashKey(KEY_BACKSPACE)
		app.startRepeat(ActionBackspace)
	case ActionBackspaceRelease:
		app.stopRepeat()
	case ActionSpace:
		if inj != nil {
			inj.PressKey(KEY_SPACE, nil)
		}
		kb.FlashKey(KEY_SPACE)
		app.startRepeat(ActionSpace)
	case ActionSpaceRelease:
		app.stopRepeat()
	case ActionEnter:
		if inj != nil {
			inj.PressKey(KEY_ENTER, nil)
		}
		kb.FlashKey(KEY_ENTER)
		app.startRepeat(ActionEnter)
	case ActionEnterRelease:
		app.stopRepeat()
	case ActionShiftOn:
		kb.ShiftActive = true
		kb.ShiftHeld = true
	case ActionShiftOff:
		kb.ShiftActive = false
		kb.ShiftHeld = false
	case ActionCapsToggle:
		kb.CapsActive = !kb.CapsActive
	case ActionMouseMove:
		if inj != nil {
			inj.MoveMouse(a.DX, a.DY)
		}
	case ActionLeftClick:
		if inj != nil {
			inj.ClickMouse(0x110, true) // BTN_LEFT press
		}
	case ActionLeftClickRelease:
		if inj != nil {
			inj.ClickMouse(0x110, false) // BTN_LEFT release
		}
	case ActionRightClick:
		if inj != nil {
			inj.ClickMouse(0x111, true) // BTN_RIGHT press
		}
	case ActionRightClickRelease:
		if inj != nil {
			inj.ClickMouse(0x111, false) // BTN_RIGHT release
		}
	case ActionPositionToggle:
		app.posTop = !app.posTop
		if app.posTop {
			rend.Flash("↑ TOP")
		} else {
			rend.Flash("↓ BOTTOM")
		}
		if hasLayerShell() {
			repositionLayerSurface(app.posTop, app.margin, app.window)
		} else {
			app.winY = app.computeY()
			SDL3SetWindowPosition(app.window, app.winX, app.winY)
		}
		SavePosition(app.posTop)
	case ActionClose:
		if app.daemon {
			// Daemon mode: close button hides instead of exiting
			app.lock.Lock()
			app.togglePending = true
			app.lock.Unlock()
			return
		}
		app.stopRepeat()
		app.running = false
		return
	}
}

// touchToPx converts normalized finger coordinates to window pixels so the
// same window-space mapping as mouse clicks can be used for both.
func (app *App) touchToPx(ev SDL3Event) (int32, int32) {
	return int32(ev.FingerX * float32(app.winW)), int32(ev.FingerY * float32(app.winH))
}

type pointerAction int

const (
	pointerDown pointerAction = iota
	pointerMove
	pointerUp
)

// handlePointer drives grid navigation from touch and mouse input: the
// highlight follows the pointer, and a key press fires on release (matching
// the gamepad A-button short-press semantics). While the accent popup is
// open, a pointer-down on it selects the accent and release confirms it.
func (app *App) handlePointer(a pointerAction, x, y int32, kb *KeyboardState, inj *Injector, rend *Renderer) {
	if !app.visible {
		return
	}
	rend.MarkDirty()

	switch a {
	case pointerDown:
		kb.ReleaseAltTab(inj)
		if kb.AccentPopup != nil {
			if idx, ok := rend.AccentCellAt(kb, x, y); ok {
				kb.AccentPopup.Selected = idx
				app.pointOnPopup = true
				return
			}
			kb.CloseAccentPopup()
		}
		app.moveCursorTo(x, y, kb, rend)
	case pointerMove:
		if kb.AccentPopup != nil && app.pointOnPopup {
			if idx, ok := rend.AccentCellAt(kb, x, y); ok {
				kb.AccentPopup.Selected = idx
			}
			return
		}
		app.moveCursorTo(x, y, kb, rend)
	case pointerUp:
		app.pointOnPopup = false
		if kb.AccentPopup != nil {
			if _, ok := rend.AccentCellAt(kb, x, y); ok {
				kb.PressCurrent(inj) // confirms the highlighted accent
				return
			}
			kb.CloseAccentPopup()
			return
		}
		if row, col, ok := rend.KeyAtWindow(kb, x, y); ok {
			kb.CursorRow = row
			kb.CursorCol = col
			kb.PressCurrent(inj)
		}
	}
}

// moveCursorTo moves the grid highlight to the key under a window pixel point.
func (app *App) moveCursorTo(x, y int32, kb *KeyboardState, rend *Renderer) {
	if row, col, ok := rend.KeyAtWindow(kb, x, y); ok {
		kb.CursorRow = row
		kb.CursorCol = col
	}
}

// computeY returns the window Y position based on current fullscreen state.
// When a fullscreen window is active, uses raw monitor bounds (screen edge).
// Otherwise uses workarea bounds (respects panels/taskbars).
func (app *App) computeY() int32 {
	area := app.posArea
	if IsFullscreenActive() {
		area = app.mon
	}
	if app.posTop {
		return area.Y + app.margin
	}
	return area.Y + area.H - app.winH - app.margin
}

func (app *App) startRepeat(action ActionType) {
	// Don't restart repeat if already repeating this action (called from repeat loop)
	if app.repeatAction == action {
		return
	}
	app.repeatAction = action
	app.repeatStart = time.Now()
	app.repeatInitial = true
}

func (app *App) stopRepeat() {
	app.repeatAction = ActionNone
}

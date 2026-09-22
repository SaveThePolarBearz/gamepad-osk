package main

/*
#cgo pkg-config: wayland-client
#include <wayland-client.h>
#include "wlr-layer-shell-client.h"
#include <stdlib.h>
#include <stdio.h>
#include <string.h>
#include <SDL3/SDL.h>
#include <SDL3/SDL_video.h>
#include <SDL3/SDL_properties.h>

// --- Layer shell state ---
static struct zwlr_layer_shell_v1 *layer_shell = NULL;
static struct zwlr_layer_surface_v1 *layer_surface = NULL;
static struct wl_surface *wl_surf = NULL;
static uint32_t configure_serial = 0;
static int configured = 0;

// --- Target output selection ---
// SDL display names (long descriptive strings) don't match wl_output names, and
// sway reports wl_output geometry positions as (0,0) on every output, so both
// position- and SDL-name-matching are unreliable. Targeting is therefore by
// wl_output NAME when configured (e.g. "DSI-1"), else the SDL primary matched
// by current-mode size.
static struct wl_output *target_output = NULL;
static char target_name[64] = ""; // configured wl_output name ("" = SDL primary)
static int target_w = 0, target_h = 0; // SDL primary pixel size (the landing screen)
static int target_tw = 0, target_th = 0; // matching wl_output current-mode size

// Track outputs during registry discovery
#define MAX_OUTPUTS 8
static struct {
    struct wl_output *output;
    int x, y;
    int tw, th; // current-mode pixel size
    int transform; // WL_OUTPUT_TRANSFORM_* (geometry event)
    int done; // geometry received
    char name[64]; // wl_output.name (sway: DSI-1, DP-1, ...)
} discovered_outputs[MAX_OUTPUTS];
static int num_outputs = 0;

static void output_geometry(void *data, struct wl_output *output,
    int32_t x, int32_t y, int32_t pw, int32_t ph,
    int32_t subpixel, const char *make, const char *model, int32_t transform) {
    (void)pw; (void)ph; (void)subpixel; (void)make; (void)model;
    // Find this output in our list and store geometry
    for (int i = 0; i < num_outputs; i++) {
        if (discovered_outputs[i].output == output) {
            discovered_outputs[i].x = x;
            discovered_outputs[i].y = y;
            discovered_outputs[i].transform = transform;
            discovered_outputs[i].done = 1;
            break;
        }
    }
}

static void output_mode(void *data, struct wl_output *output,
    uint32_t flags, int32_t w, int32_t h, int32_t refresh) {
    (void)data; (void)output; (void)refresh;
    // wl_output.mode flag WLR_OUTPUT_MODE_CURRENT == 1: only the current
    // mode is the output's real pixel size (preferred/offered modes aren't).
    if (flags & 1) {
        for (int i = 0; i < num_outputs; i++) {
            if (discovered_outputs[i].output == output) {
                discovered_outputs[i].tw = w;
                discovered_outputs[i].th = h;
                break;
            }
        }
    }
}
static void output_done(void *data, struct wl_output *output) {
    (void)data; (void)output;
}
static void output_scale(void *data, struct wl_output *output, int32_t factor) {
    (void)data; (void)output; (void)factor;
}
static void output_name(void *data, struct wl_output *output, const char *name) {
    (void)data;
    for (int i = 0; i < num_outputs; i++) {
        if (discovered_outputs[i].output == output) {
            snprintf(discovered_outputs[i].name, sizeof(discovered_outputs[i].name), "%s", name);
            break;
        }
    }
}
static void output_description(void *data, struct wl_output *output, const char *desc) {
    (void)data; (void)output; (void)desc;
}

static const struct wl_output_listener output_listener = {
    .geometry = output_geometry,
    .mode = output_mode,
    .done = output_done,
    .scale = output_scale,
    .name = output_name,
    .description = output_description,
};

// --- Registry listener: discover layer_shell and outputs ---
static void registry_global(void *data, struct wl_registry *registry,
    uint32_t name, const char *interface, uint32_t version) {
    (void)data;
    if (strcmp(interface, zwlr_layer_shell_v1_interface.name) == 0) {
        layer_shell = wl_registry_bind(registry, name,
            &zwlr_layer_shell_v1_interface, version < 4 ? version : 4);
    }
    if (strcmp(interface, "wl_output") == 0 && num_outputs < MAX_OUTPUTS) {
        struct wl_output *out = wl_registry_bind(registry, name,
            &wl_output_interface, version < 4 ? version : 4);
        discovered_outputs[num_outputs].output = out;
        discovered_outputs[num_outputs].done = 0;
        discovered_outputs[num_outputs].name[0] = '\0';
        wl_output_add_listener(out, &output_listener, NULL);
        num_outputs++;
    }
}

static void registry_global_remove(void *data, struct wl_registry *registry, uint32_t name) {
    (void)data; (void)registry; (void)name;
}

static const struct wl_registry_listener registry_listener = {
    .global = registry_global,
    .global_remove = registry_global_remove,
};

// --- Layer surface listener ---
static void layer_surface_configure(void *data,
    struct zwlr_layer_surface_v1 *surface,
    uint32_t serial, uint32_t width, uint32_t height) {
    (void)data; (void)width; (void)height;
    configure_serial = serial;
    configured = 1;
    zwlr_layer_surface_v1_ack_configure(surface, serial);
}

static void layer_surface_closed(void *data,
    struct zwlr_layer_surface_v1 *surface) {
    (void)data; (void)surface;
}

static const struct zwlr_layer_surface_v1_listener layer_surface_listener = {
    .configure = layer_surface_configure,
    .closed = layer_surface_closed,
};

// Select the target output: by configured name when set, else by SDL-primary
// current-mode size.
static void select_target_output() {
    target_output = NULL;
    target_tw = 0;
    target_th = 0;
    if (target_name[0]) {
        for (int i = 0; i < num_outputs; i++) {
            if (discovered_outputs[i].output &&
                strcmp(discovered_outputs[i].name, target_name) == 0) {
                target_output = discovered_outputs[i].output;
                target_tw = discovered_outputs[i].tw;
                target_th = discovered_outputs[i].th;
                return;
            }
        }
        return;
    }
    if (target_w <= 0 || target_h <= 0) return;
    for (int i = 0; i < num_outputs; i++) {
        if (discovered_outputs[i].output &&
            discovered_outputs[i].tw == target_w &&
            discovered_outputs[i].th == target_h) {
            target_output = discovered_outputs[i].output;
            target_tw = discovered_outputs[i].tw;
            target_th = discovered_outputs[i].th;
            return;
        }
    }
}

// Discover layer_shell global and outputs. Only called once (first attach).
// Safe to roundtrip here because no renderer exists yet.
static int discover_globals(SDL_Window *sdl_win) {
    SDL_PropertiesID gprops = SDL_GetGlobalProperties();
    struct wl_display *display = SDL_GetPointerProperty(gprops,
        SDL_PROP_GLOBAL_VIDEO_WAYLAND_WL_DISPLAY_POINTER, NULL);
    if (!display) return -2;

    struct wl_registry *registry = wl_display_get_registry(display);
    wl_registry_add_listener(registry, &registry_listener, NULL);
    wl_display_roundtrip(display); // discover globals
    wl_display_roundtrip(display); // receive output geometry events

    wl_registry_destroy(registry);

    if (!layer_shell) return -1;

    select_target_output();
    return 0;
}

// --- Temp output enumeration for pre-window sizing (see lookup_target_output_size).
// Fully isolated from the layer-shell/discovery statics used by the SDL attach path.
static struct wl_output *tmp_outs[MAX_OUTPUTS];
static int tmp_n = 0;
static int tmp_w[MAX_OUTPUTS], tmp_h[MAX_OUTPUTS], tmp_tr[MAX_OUTPUTS];
static char tmp_nm[MAX_OUTPUTS][64];

static void tmp_geometry(void *data, struct wl_output *out,
    int32_t ox, int32_t oy, int32_t pw, int32_t ph,
    int32_t sp, const char *mk, const char *md, int32_t tr) {
    (void)data; (void)out; (void)ox; (void)oy;
    (void)pw; (void)ph; (void)sp; (void)mk; (void)md;
    for (int i = 0; i < tmp_n; i++) {
        if (tmp_outs[i] == out) {
            tmp_tr[i] = tr;
            break;
        }
    }
}
static void tmp_mode(void *data, struct wl_output *out,
    uint32_t flags, int32_t mw, int32_t mh, int32_t ref) {
    (void)data; (void)out; (void)ref;
    if (flags & 1) { // WLR_OUTPUT_MODE_CURRENT
        for (int i = 0; i < tmp_n; i++) {
            if (tmp_outs[i] == out) {
                tmp_w[i] = mw;
                tmp_h[i] = mh;
                break;
            }
        }
    }
}
static void tmp_done(void *data, struct wl_output *out) { (void)data; (void)out; }
static void tmp_scale(void *data, struct wl_output *out, int32_t s) { (void)data; (void)out; (void)s; }
static void tmp_name(void *data, struct wl_output *out, const char *n) {
    (void)data;
    for (int i = 0; i < tmp_n; i++) {
        if (tmp_outs[i] == out) {
            snprintf(tmp_nm[i], sizeof(tmp_nm[i]), "%s", n);
            break;
        }
    }
}
static void tmp_desc(void *data, struct wl_output *out, const char *d) { (void)data; (void)out; (void)d; }

static const struct wl_output_listener tmp_listener = {
    .geometry = tmp_geometry,
    .mode = tmp_mode,
    .done = tmp_done,
    .scale = tmp_scale,
    .name = tmp_name,
    .description = tmp_desc,
};

static void tmp_registry(void *data, struct wl_registry *reg,
    uint32_t name, const char *iface, uint32_t ver) {
    (void)data; (void)name; (void)ver;
    if (strcmp(iface, wl_output_interface.name) == 0 && tmp_n < MAX_OUTPUTS) {
        struct wl_output *out = wl_registry_bind(reg, name,
            &wl_output_interface, ver < 4 ? ver : 4);
        tmp_outs[tmp_n] = out;
        tmp_nm[tmp_n][0] = '\0';
        wl_output_add_listener(out, &tmp_listener, NULL);
        tmp_n++;
    }
}
static void tmp_registry_remove(void *data, struct wl_registry *reg, uint32_t name) {
    (void)data; (void)reg; (void)name;
}
static const struct wl_registry_listener tmp_registry_listener = {
    .global = tmp_registry,
    .global_remove = tmp_registry_remove,
};

// Open the short-lived Wayland connection and enumerate outputs (sizes + names).
// Returns 1 with *dpy/*reg set (caller must tmp_cleanup), 0 on failure.
static int tmp_enumerate(struct wl_display **dpy, struct wl_registry **reg) {
    *dpy = wl_display_connect(NULL);
    if (!*dpy) return 0;
    tmp_outs[0] = NULL;
    tmp_n = 0;
    *reg = wl_display_get_registry(*dpy);
    wl_registry_add_listener(*reg, &tmp_registry_listener, NULL);
    wl_display_roundtrip(*dpy);
    wl_display_roundtrip(*dpy);
    return 1;
}

static void tmp_cleanup(struct wl_display *dpy, struct wl_registry *registry) {
    for (int i = 0; i < tmp_n; i++) {
        wl_output_destroy(tmp_outs[i]);
    }
    wl_registry_destroy(registry);
    wl_display_disconnect(dpy);
}

// Look up a wl_output's current-mode pixel size by NAME (e.g. "DSI-1") or by
// size (SDL primary WxH). These run before any window exists via a short
// separate Wayland connection, leaving the layer-shell statics untouched.
// Returns 1 and sets *ow/*oh on a match, else 0.
static int lookup_named_output_size(const char *name, int *ow, int *oh, int *otr) {
    struct wl_display *dpy;
    struct wl_registry *reg;
    if (!tmp_enumerate(&dpy, &reg) || !name || !name[0]) {
        if (dpy) tmp_cleanup(dpy, reg);
        return 0;
    }
    int found = 0;
    for (int i = 0; i < tmp_n; i++) {
        if (tmp_nm[i][0] && strcmp(tmp_nm[i], name) == 0 && tmp_w[i] > 0) {
            *ow = tmp_w[i];
            *oh = tmp_h[i];
            *otr = tmp_tr[i];
            found = 1;
            break;
        }
    }
    tmp_cleanup(dpy, reg);
    return found;
}

static int lookup_target_output_size(int w, int h, int *ow, int *oh, int *otr) {
    struct wl_display *dpy;
    struct wl_registry *reg;
    if (!tmp_enumerate(&dpy, &reg) || w <= 0 || h <= 0) {
        if (dpy) tmp_cleanup(dpy, reg);
        return 0;
    }
    int found = 0;
    for (int i = 0; i < tmp_n; i++) {
        if (tmp_w[i] == w && tmp_h[i] == h && tmp_w[i] > 0) {
            *ow = tmp_w[i];
            *oh = tmp_h[i];
            *otr = tmp_tr[i];
            found = 1;
            break;
        }
    }
    tmp_cleanup(dpy, reg);
    return found;
}

// Attach layer-shell role asynchronously (no roundtrip wait).
// Configure arrives via SDL's event pump. Check is_layer_shell_ready() before rendering.
// Returns 0 on success, -1 if layer_shell not available, -2 on other error.
static int attach_layer_shell_async(SDL_Window *sdl_win, int width, int height,
    int anchor_top, int margin, int exclusive_zone) {

    // Get wl_display from SDL3
    SDL_PropertiesID gprops = SDL_GetGlobalProperties();
    struct wl_display *display = SDL_GetPointerProperty(gprops,
        SDL_PROP_GLOBAL_VIDEO_WAYLAND_WL_DISPLAY_POINTER, NULL);
    if (!display) return -2;

    // Get wl_surface from SDL3 window (only on first call, SDL owns it)
    if (!wl_surf) {
        SDL_PropertiesID wprops = SDL_GetWindowProperties(sdl_win);
        wl_surf = SDL_GetPointerProperty(wprops,
            SDL_PROP_WINDOW_WAYLAND_SURFACE_POINTER, NULL);
        if (!wl_surf) return -2;
    }

    // Discover globals on first call only
    if (!layer_shell) {
        int ret = discover_globals(sdl_win);
        if (ret != 0) return ret;
    }

    // Re-select the target output each attach so a live `output <name>` IPC
    // switch takes effect on the re-attach below.
    select_target_output();

    // Destroy existing layer surface if re-attaching (e.g. daemon toggle)
    if (layer_surface) {
        zwlr_layer_surface_v1_destroy(layer_surface);
        layer_surface = NULL;
        configured = 0;
    }

    // Clear any pending EGL buffer state before attaching layer-shell role.
    // SDL's eglSwapBuffers may have queued a buffer on the wl_surface from
    // a previous frame. Attaching NULL ensures the commit below doesn't
    // carry a stale buffer that would violate the layer-shell protocol.
    wl_surface_attach(wl_surf, NULL, 0, 0);
    wl_surface_commit(wl_surf);

    // Create layer surface on target output
    layer_surface = zwlr_layer_shell_v1_get_layer_surface(
        layer_shell, wl_surf, target_output,
        ZWLR_LAYER_SHELL_V1_LAYER_OVERLAY, "gamepad-osk");
    if (!layer_surface) return -2;

    zwlr_layer_surface_v1_add_listener(layer_surface, &layer_surface_listener, NULL);

    // Configure anchors, size, margins.
    // Anchor only TOP or BOTTOM (no horizontal anchors) so the compositor
    // centers the surface. KDE left-aligns when LEFT|RIGHT are set with
    // an explicit width; omitting them gives consistent centering everywhere.
    uint32_t anchor = 0;
    if (anchor_top) {
        anchor = ZWLR_LAYER_SURFACE_V1_ANCHOR_TOP;
        zwlr_layer_surface_v1_set_margin(layer_surface, margin, 0, 0, 0);
    } else {
        anchor = ZWLR_LAYER_SURFACE_V1_ANCHOR_BOTTOM;
        zwlr_layer_surface_v1_set_margin(layer_surface, 0, 0, margin, 0);
    }
    zwlr_layer_surface_v1_set_anchor(layer_surface, anchor);
    zwlr_layer_surface_v1_set_size(layer_surface, width, height);

    zwlr_layer_surface_v1_set_keyboard_interactivity(layer_surface,
        ZWLR_LAYER_SURFACE_V1_KEYBOARD_INTERACTIVITY_NONE);
    zwlr_layer_surface_v1_set_exclusive_zone(layer_surface, exclusive_zone);

    // Commit to trigger configure -- DO NOT WAIT (no roundtrip)
    configured = 0;
    wl_surface_commit(wl_surf);
    wl_display_flush(display);

    return 0;
}

// Reposition the layer surface (top/bottom toggle).
static void reposition_layer_surface(int anchor_top, int margin,
    SDL_Window *sdl_win) {
    if (!layer_surface) return;

    uint32_t anchor = 0;
    if (anchor_top) {
        anchor = ZWLR_LAYER_SURFACE_V1_ANCHOR_TOP;
        zwlr_layer_surface_v1_set_margin(layer_surface, margin, 0, 0, 0);
    } else {
        anchor = ZWLR_LAYER_SURFACE_V1_ANCHOR_BOTTOM;
        zwlr_layer_surface_v1_set_margin(layer_surface, 0, 0, margin, 0);
    }
    zwlr_layer_surface_v1_set_anchor(layer_surface, anchor);

    // Get wl_display for commit
    SDL_PropertiesID gprops = SDL_GetGlobalProperties();
    struct wl_display *display = SDL_GetPointerProperty(gprops,
        SDL_PROP_GLOBAL_VIDEO_WAYLAND_WL_DISPLAY_POINTER, NULL);
    if (wl_surf) {
        wl_surface_commit(wl_surf);
    }
    if (display) {
        wl_display_flush(display);
    }
}

// Destroy the layer surface (hide or cleanup).
// Preserves layer_shell global and wl_surf for re-attach.
static void destroy_layer_surface() {
    if (layer_surface) {
        zwlr_layer_surface_v1_destroy(layer_surface);
        layer_surface = NULL;
    }
    configured = 0;
}

// Full cleanup on exit.
static void cleanup_layer_shell() {
    destroy_layer_surface();
    if (layer_shell) {
        zwlr_layer_shell_v1_destroy(layer_shell);
        layer_shell = NULL;
    }
    for (int i = 0; i < num_outputs; i++) {
        if (discovered_outputs[i].output) {
            wl_output_destroy(discovered_outputs[i].output);
            discovered_outputs[i].output = NULL;
        }
    }
    num_outputs = 0;
    target_output = NULL;
    wl_surf = NULL;
}

// Check if layer shell is configured and ready for rendering.
static int is_layer_shell_ready() {
    return layer_surface != NULL && configured;
}

// Check if layer shell was attached (may not be configured yet).
static int has_layer_shell() {
    return layer_surface != NULL;
}

// Set the target output size (SDL primary bounds) for wl_output matching.
static void set_target_size(int w, int h) {
    target_w = w;
    target_h = h;
    target_name[0] = '\0'; // size-match supersedes any stale configured name
}
static void set_target_name(const char *name) {
    if (!name) { target_name[0] = '\0'; return; }
    snprintf(target_name, sizeof(target_name), "%s", name);
}
*/
import "C"
import (
	"log"
	"unsafe"
)

var layerShellActive bool

// setTargetSize selects the wl_output to attach layer-shell to, by current-mode
// size matching the SDL primary bounds.
func setTargetSize(w, h int32) {
	C.set_target_size(C.int(w), C.int(h))
}

// setTargetName selects the wl_output to attach layer-shell to, by name
// (e.g. "DSI-1", "DP-1" -- see `swaymsg -t get_outputs`).
func setTargetName(name string) {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	C.set_target_name(cname)
}

// getNamedOutputInfo finds a wl_output named `name` and returns its LOGICAL
// (post-transform) pixel size, so the keyboard spans the width the user sees
// on the target screen (a transform-90/270 output's logical width is its long
// edge). The wl_output mode event reports the physical size; the transform
// decides which axis is logical width. Works before the window exists via a
// short separate Wayland connection. ok=false if not on Wayland or no output.
func getNamedOutputInfo(name string) (int32, int32, bool) {
	if name == "" {
		return 0, 0, false
	}
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	var ow, oh, otr C.int
	ok := C.lookup_named_output_size(cname, &ow, &oh, &otr) != 0
	if !ok {
		return 0, 0, false
	}
	lw, lh := int32(ow), int32(oh)
	if otr == 1 || otr == 3 { // 90 or 270: logical axes are swapped
		lw, lh = lh, lw
	}
	return lw, lh, true
}

// getTargetOutputInfo finds a wl_output whose physical current-mode size equals
// (w, h) -- the SDL primary the OSK will land on -- and returns its LOGICAL
// (post-transform) size. ok=false if not on Wayland or no match.
func getTargetOutputInfo(w, h int32) (int32, int32, bool) {
	if w <= 0 || h <= 0 {
		return 0, 0, false
	}
	var ow, oh, otr C.int
	ok := C.lookup_target_output_size(C.int(w), C.int(h), &ow, &oh, &otr) != 0
	if !ok {
		return 0, 0, false
	}
	lw, lh := int32(ow), int32(oh)
	if otr == 1 || otr == 3 { // 90 or 270: logical axes are swapped
		lw, lh = lh, lw
	}
	return lw, lh, true
}

// createWaylandWindow creates an SDL3 window with a roleless Wayland surface.
func createWaylandWindow(title string, w, h int32) (*Window, error) {
	window, err := SDL3CreateWindowWithProps(title, 0, 0, w, h,
		true,  // hidden
		true,  // borderless
		false, // always_on_top (layer-shell handles this)
		true,  // waylandRoleCustom
	)
	if err != nil {
		// Fallback: standard window
		log.Printf("Warning: roleless window creation failed: %v, falling back to standard window", err)
		return SDL3CreateWindowWithProps(title, 0, 0, w, h,
			true, true, true, false)
	}
	return window, nil
}

// attachLayerShellAsync attaches a wlr-layer-shell overlay role asynchronously.
// Configure arrives via SDL's event pump. Check IsLayerShellReady() before rendering.
// mode: "overlay" (exclusive_zone 0, floats over windows) or "shift" (reserve the
// surface's own strip so tiled windows move aside). Sway 1.12 only honors
// exclusive_zone > 0 when shrinking the usable area -- -1 is treated as floating.
func attachLayerShellAsync(window *Window, w, h int32, top bool, margin int32, mode string) {
	anchorTop := 0
	if top {
		anchorTop = 1
	}
	exclusive := 0
	if mode == "shift" {
		exclusive = int(h) + int(margin)
	}
	ret := int(C.attach_layer_shell_async(
		(*C.SDL_Window)(window.ptr),
		C.int(w), C.int(h),
		C.int(anchorTop), C.int(margin), C.int(exclusive),
	))

	switch ret {
	case 0:
		layerShellActive = true
		log.Printf("Layer-shell overlay requested (waiting for configure)")
	case -1:
		log.Printf("Warning: compositor does not support wlr-layer-shell, using standard window")
		layerShellActive = false
	default:
		log.Printf("Warning: failed to attach layer-shell (error %d), using standard window", ret)
		layerShellActive = false
	}
}

// IsLayerShellReady returns true if the layer surface is configured and ready for rendering.
func IsLayerShellReady() bool {
	return bool(C.is_layer_shell_ready() != 0)
}

// repositionLayerSurface changes the layer surface anchor between top and bottom.
func repositionLayerSurface(top bool, margin int32, window *Window) {
	if !layerShellActive {
		return
	}
	anchorTop := 0
	if top {
		anchorTop = 1
	}
	C.reposition_layer_surface(C.int(anchorTop), C.int(margin),
		(*C.SDL_Window)(window.ptr))
}

// destroyLayerSurface destroys the layer surface (for hide).
// Preserves layer_shell global for re-attach.
func destroyLayerSurface() {
	if layerShellActive {
		C.destroy_layer_surface()
		layerShellActive = false
	}
}

// cleanupLayerShell fully cleans up all layer-shell resources (exit).
func cleanupLayerShell() {
	C.cleanup_layer_shell()
	layerShellActive = false
}

// hasLayerShell returns true if layer-shell was attached (may not be configured).
func hasLayerShell() bool {
	return layerShellActive
}

package main

/*
#include <errno.h>
#include <linux/input.h>
#include <string.h>
#include <sys/ioctl.h>
#include <unistd.h>

// Upload one FF_CONSTANT effect, return its id, or -errno on failure.
//
// The ioctl lives in C on purpose. struct ff_effect is 48 bytes with the union
// at offset 16, but a Go struct with the same field list has no padding, so it
// comes out 46 bytes and every field after `direction` is two bytes off. An
// earlier hand-rolled version had the right total size yet wrote replay.length
// where the kernel reads replay.delay. Letting the compiler derive both the
// layout and the EVIOCSFF number from <linux/input.h> removes that whole class
// of bug rather than fixing one instance of it.
//
// FF_CONSTANT is the only effect this driver really implements.
// haptics_upload_effect() switches on FF_CONSTANT and FF_PERIODIC with no
// default arm, so an FF_RUMBLE upload falls out of the switch, sets
// chip_effect_loaded, and returns 0: it reports success while programming
// nothing, so playback is a silent no-op. FF_PERIODIC is no escape either, the
// driver sets no waveform bits in ffbit, so ff-core rejects FF_SINE with
// -EINVAL before the driver is reached. Note that
// haptics_upload_ff_periodic_sine() just wraps FF_CONSTANT anyway, using
// magnitude as the level, so going direct skips a broken layer.

static int thorch_ff_upload_constant(int fd, int length_ms, int level) {
	struct ff_effect eff;

	memset(&eff, 0, sizeof(eff));
	eff.type = FF_CONSTANT;
	eff.id = -1; // ask the kernel to allocate a fresh slot
	eff.replay.length = (unsigned short)length_ms;
	eff.replay.delay = 0;
	eff.u.constant.level = (short)level;

	if (ioctl(fd, EVIOCSFF, &eff) < 0)
		return -errno;

	return eff.id;
}

static int thorch_ff_play(int fd, int id) {
	struct input_event ev;

	memset(&ev, 0, sizeof(ev));
	ev.type = EV_FF;
	ev.code = (unsigned short)id;
	ev.value = 1;

	if (write(fd, &ev, sizeof(ev)) != (ssize_t)sizeof(ev))
		return -errno;

	return 0;
}
*/
import "C"

import (
	"log"
	"sync"
	"syscall"
	"time"
)

// Haptics drives the Thor motor ("qcom-hv-haptics", /dev/input/event2) through
// the evdev force-feedback interface. The OSK only buzzes on pointer-initiated
// key presses (touchscreen tap or mouse click); gamepad-driven key presses go
// through handleAction and never trigger it. If the node is unavailable the buzz
// is dropped silently (log once, back off 60s) so a missing motor never
// interrupts typing.
type Haptics struct {
	path     string
	fd       int
	effectID int
	level    int

	mu       sync.Mutex
	lastFail time.Time // open/upload/play failure time for back-off + one-shot log
}

const (
	buzzLen     = 40 // ms, short enough to feel like a key-tap
	failBackoff = 60 * time.Second

	// DefaultBuzzLevel is used when [haptics] level is absent or unparseable.
	// 0x5f00 = 24320, which is ~74% of maxBuzzLevel. An earlier comment here
	// claimed "~25%"; that was wrong, so do not trust it over this number.
	DefaultBuzzLevel = 0x5f00
	maxBuzzLevel     = 0x7fff
)

func NewHaptics(path string, level int) *Haptics {
	if level < 0 {
		level = 0
	}
	if level > maxBuzzLevel {
		level = maxBuzzLevel
	}
	return &Haptics{path: path, fd: -1, level: level, lastFail: time.Time{}}
}

// open lazily opens the FF device and uploads the single constant effect.
func (h *Haptics) open() error {
	fd, err := syscall.Open(h.path, syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}

	id := int(C.thorch_ff_upload_constant(C.int(fd), C.int(buzzLen), C.int(h.level)))
	if id < 0 {
		syscall.Close(fd)
		return syscall.Errno(-id)
	}

	h.fd = fd
	h.effectID = id
	return nil
}

// Buzz plays the uploaded effect once. Nil-safe so callers can pass a lazy
// *Haptics that may never have been initialized.
func (h *Haptics) Buzz() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.fd < 0 {
		if time.Since(h.lastFail) < failBackoff {
			return
		}
		if err := h.open(); err != nil {
			h.lastFail = time.Now()
			log.Printf("haptics: motor %s unavailable (%v) - touch buzz disabled", h.path, err)
			return
		}
	}

	if rc := int(C.thorch_ff_play(C.int(h.fd), C.int(h.effectID))); rc < 0 {
		h.lastFail = time.Now()
		log.Printf("haptics: play failed (%v) - touch buzz disabled", syscall.Errno(-rc))
	}
}

// Close releases the device (called on exit; the kernel auto-stops effects).
func (h *Haptics) Close() {
	if h == nil || h.fd < 0 {
		return
	}
	syscall.Close(h.fd)
	h.fd = -1
}

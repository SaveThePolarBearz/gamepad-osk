package main

import (
	"encoding/binary"
	"log"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Haptics drives the Thor motor ("qcom-hv-haptics", /dev/input/event2) through
// the evdev force-feedback interface. The OSK only buzzes on pointer-initiated
// key presses (touchscreen tap or mouse click); gamepad-driven key presses go
// through handleAction and never trigger it. Access to the device comes from
// the same udev input/uaccess rule the gamepad already relies on; if the node
// is unavailable the buzz is dropped silently (log once, back off 60s) so a
// missing motor never interrupts typing.
type Haptics struct {
	path     string
	fd       int
	effectID int
	uploaded bool

	mu       sync.Mutex
	lastFail time.Time // open/write failure time for back-off + one-shot log
}

const (
	evFF     = 0x15 // EV_FF: force feedback events
	ffRumble = 0x50 // FF_RUMBLE: strong + weak motors
	buzzLen  = 25   // ms, short enough to feel like a key-tap, not a rumble
	buzzMag  = 0x6000 // 0..0xffff motor magnitude (roughly 60%)
	evIocWrite = 1 << 30
	failBackoff = 60 * time.Second
)

// ffEffect mirrors struct ff_effect from linux/input.h. The union must be
// 32 bytes so the whole struct is 48 (C sizeof on the kernel ABI); the rumble
// fields live at union offset 0 (bytes 16..20 of the struct).
type ffEffect struct {
	Type, ID, Checksum, Direction uint16
	Trigger                       struct{ Button, Interval uint16 }
	Replay                        struct{ Length, Delay uint16 }
	U                             [16]uint16 // union (strong/weak at U[0..1])
}

// inputEvent mirrors struct input_event (24 bytes) for EV_FF play/stop writes.
type inputEvent struct {
	Sec, Usec int64
	Type      uint16
	Code      uint16
	Value     int32
}

// iow encodes _IOW(type, nr, size) as generic ioctl (dir=write, 1<<30).
func iow(t, nr byte, size uintptr) uintptr {
	return evIocWrite | (size << 16) | (uintptr(t) << 8) | uintptr(nr)
}

func NewHaptics(path string) *Haptics {
	return &Haptics{path: path, fd: -1, lastFail: time.Now()}
}

// open lazily opens the FF device and uploads a single rumble effect.
func (h *Haptics) open() error {
	fd, err := syscall.Open(h.path, syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}

	var eff ffEffect
	eff.Type = ffRumble           // kernel fills eff.ID back to us
	eff.Replay.Length = buzzLen   // auto-stop after buzzLen ms
	eff.U[0] = buzzMag            // strong motor
	eff.U[1] = buzzMag            // weak motor

	var uploadErr error
	for attempt := 0; attempt < 5; attempt++ {
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), iow('E', 0x80, unsafe.Sizeof(eff)), uintptr(unsafe.Pointer(&eff)))
		if errno == 0 {
			h.effectID = int(eff.ID)
			h.uploaded = true
			break
		}
		uploadErr = errno
		if errno == syscall.EBUSY { // effect table transiently full
			time.Sleep(10 * time.Millisecond)
			continue
		}
		break
	}
	if !h.uploaded {
		syscall.Close(fd)
		return uploadErr
	}
	h.fd = fd
	return nil
}

// Buzz plays the uploaded rumble once. Nil-safe so callers can pass a lazy
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

	ev := inputEvent{Type: evFF, Code: uint16(h.effectID), Value: 1}
	var buf [24]byte
	binary.LittleEndian.PutUint64(buf[0:], uint64(ev.Sec))
	binary.LittleEndian.PutUint64(buf[8:], uint64(ev.Usec))
	binary.LittleEndian.PutUint16(buf[16:], ev.Type)
	binary.LittleEndian.PutUint16(buf[18:], ev.Code)
	binary.LittleEndian.PutUint32(buf[20:], uint32(ev.Value))
	if n, err := syscall.Write(h.fd, buf[:]); n != len(buf) || err != nil {
		h.lastFail = time.Now()
		log.Printf("haptics: write failed (%v) - touch buzz disabled", err)
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
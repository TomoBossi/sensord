//go:build android

// Package ndk is the hub.Backend for real devices, on top of the NDK sensor
// API (ASensorManager). It must run inside an app process that the system
// treats as foreground, or continuous sensors stop when the screen goes off.
package ndk

/*
// Termux clang targets an older API level; these symbols exist on the API 29+
// devices the app supports (minSdkVersion 29), so declare them weak.
#cgo CFLAGS: -D__ANDROID_UNAVAILABLE_SYMBOLS_ARE_WEAK__ -Wno-unguarded-availability
#cgo LDFLAGS: -landroid
#include <stdlib.h>
#include <android/looper.h>
#include <android/sensor.h>

static uint64_t step_count(const ASensorEvent *e) { return e->u64.step_counter; }
static const float *event_data(const ASensorEvent *e) { return e->data; }
*/
import "C"

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"unsafe"

	"github.com/TomoBossi/sensord/internal/hub"
)

// Backend implements hub.Backend. Set Dispatch before enabling any sensor.
type Backend struct {
	Dispatch func(handle int32, t int64, v []float64)

	sm      *C.ASensorManager
	q       *C.ASensorEventQueue
	byH     map[int32]*C.ASensor
	nvals   map[int32]int // values per event, by handle; 0 = unknown (vendor type)
	infos   []hub.Info
	started chan error
}

// Open connects to the sensor service as package pkg and starts the event loop.
func Open(pkg string) (*Backend, error) {
	b := &Backend{byH: map[int32]*C.ASensor{}, nvals: map[int32]int{}, started: make(chan error, 1)}
	cpkg := C.CString(pkg)
	b.sm = C.ASensorManager_getInstanceForPackage(cpkg)
	C.free(unsafe.Pointer(cpkg))
	if b.sm == nil {
		return nil, errors.New("no sensor manager")
	}
	b.scan()
	go b.loop()
	if err := <-b.started; err != nil {
		return nil, err
	}
	return b, nil
}

func (b *Backend) scan() {
	var list C.ASensorList
	n := int(C.ASensorManager_getSensorList(b.sm, &list))
	sensors := unsafe.Slice(list, n)
	for _, s := range sensors {
		h := int32(C.ASensor_getHandle(s))
		typ := int32(C.ASensor_getType(s))
		str := C.GoString(C.ASensor_getStringType(s))
		info := hub.Info{
			Handle:     h,
			Name:       C.GoString(C.ASensor_getName(s)),
			Vendor:     C.GoString(C.ASensor_getVendor(s)),
			Type:       strings.TrimPrefix(str, "android.sensor."),
			TypeID:     typ,
			MinDelayUs: int32(C.ASensor_getMinDelay(s)),
			Wakeup:     bool(C.ASensor_isWakeUpSensor(s)),
			Default:    C.ASensorManager_getDefaultSensor(b.sm, C.int(typ)) == s,
		}
		switch C.ASensor_getReportingMode(s) {
		case C.AREPORTING_MODE_CONTINUOUS:
			info.Mode = hub.Continuous
		case C.AREPORTING_MODE_ON_CHANGE:
			info.Mode = hub.OnChange
		case C.AREPORTING_MODE_ONE_SHOT:
			info.Mode = hub.OneShot
		default:
			info.Mode = hub.Special
		}
		b.byH[h] = s
		b.nvals[h] = valueCount[typ]
		b.infos = append(b.infos, info)
	}
}

func (b *Backend) loop() {
	// An ALooper belongs to one thread, so the queue and its polling stay on
	// this locked OS thread for the life of the process.
	runtime.LockOSThread()
	lp := C.ALooper_prepare(C.ALOOPER_PREPARE_ALLOW_NON_CALLBACKS)
	b.q = C.ASensorManager_createEventQueue(b.sm, lp, 1, nil, nil)
	if b.q == nil {
		b.started <- errors.New("createEventQueue failed")
		return
	}
	b.started <- nil

	var buf [32]C.ASensorEvent
	vals := make([]float64, 0, 16)
	for {
		C.ALooper_pollOnce(-1, nil, nil, nil)
		for {
			n := int(C.ASensorEventQueue_getEvents(b.q, &buf[0], C.size_t(len(buf))))
			if n <= 0 {
				break
			}
			for i := 0; i < n; i++ {
				e := &buf[i]
				h := int32(e.sensor)
				vals = vals[:0]
				if e._type == C.ASENSOR_TYPE_STEP_COUNTER {
					vals = append(vals, float64(C.step_count(e)))
				} else {
					data := unsafe.Slice(C.event_data(e), 16)
					k := b.nvals[h]
					if k == 0 { // vendor type: drop trailing zeros
						k = 16
						for k > 1 && data[k-1] == 0 {
							k--
						}
					}
					for _, x := range data[:k] {
						vals = append(vals, float64(x))
					}
				}
				b.Dispatch(h, int64(e.timestamp), vals)
			}
		}
	}
}

func (b *Backend) Sensors() []hub.Info { return b.infos }

func (b *Backend) Enable(h int32, periodUs int32) error {
	s := b.byH[h]
	if s == nil {
		return fmt.Errorf("no sensor with handle %d", h)
	}
	if r := C.ASensorEventQueue_registerSensor(b.q, s, C.int32_t(periodUs), 0); r < 0 {
		return fmt.Errorf("register %s: error %d", C.GoString(C.ASensor_getName(s)), int(r))
	}
	return nil
}

func (b *Backend) SetPeriod(h int32, periodUs int32) error {
	if r := C.ASensorEventQueue_setEventRate(b.q, b.byH[h], C.int32_t(periodUs)); r < 0 {
		return fmt.Errorf("set rate: error %d", int(r))
	}
	return nil
}

func (b *Backend) Disable(h int32) error {
	if r := C.ASensorEventQueue_disableSensor(b.q, b.byH[h]); r < 0 {
		return fmt.Errorf("disable: error %d", int(r))
	}
	return nil
}

// valueCount is how many floats of ASensorEvent.data each standard sensor
// type fills, per the Android SensorEvent documentation.
var valueCount = map[int32]int{
	1: 3, 2: 3, 3: 3, 4: 3, 5: 1, 6: 1, 8: 1, 9: 3, 10: 3,
	11: 5, // rotation vector: x, y, z, w, heading accuracy
	12: 1, 13: 1,
	14: 6, // magnetic field uncalibrated: x, y, z + bias
	15: 4, // game rotation vector
	16: 6, // gyroscope uncalibrated
	17: 1, 18: 1,
	20: 5, // geomagnetic rotation vector
	21: 1, 22: 1, 23: 1, 24: 1, 25: 1, 26: 1, 27: 1,
	28: 15, // pose 6dof
	29: 1, 30: 1, 31: 1, 34: 1,
	35: 6,                      // accelerometer uncalibrated
	36: 1,                      // hinge angle
	37: 6,                      // head tracker
	38: 6, 39: 6, 40: 9, 41: 9, // limited axes
	42: 2, // heading
}

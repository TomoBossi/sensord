// Command libsensord is the native core of the sensord app, built with
// -buildmode=c-shared and loaded by the Java service through JNI.
//
// Spike version: streams the accelerometer at 100 Hz as JSON lines to every
// client connected to 127.0.0.1:47474.
package main

/*
#cgo LDFLAGS: -landroid -llog
#include <jni.h>
#include <stdlib.h>
#include <android/log.h>
#include <android/looper.h>
#include <android/sensor.h>

static void logi(const char *msg) { __android_log_write(ANDROID_LOG_INFO, "sensord", msg); }
*/
import "C"

import (
	"fmt"
	"net"
	"runtime"
	"sync"
	"unsafe"
)

func logf(format string, args ...any) {
	s := C.CString(fmt.Sprintf(format, args...))
	C.logi(s)
	C.free(unsafe.Pointer(s))
}

// Loopback only. Unix sockets are not an option: SELinux gives every app its
// own MLS categories, which blocks connecting across apps (Termux -> us).
const addr = "127.0.0.1:47474"

var (
	mu      sync.Mutex
	clients = map[chan []byte]struct{}{}
)

func broadcast(line []byte) {
	mu.Lock()
	defer mu.Unlock()
	for ch := range clients {
		select {
		case ch <- line:
		default: // slow client: drop rather than block the sensor loop
		}
	}
}

func serve() {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		logf("listen: %v", err)
		return
	}
	logf("listening on %s", addr)
	for {
		conn, err := ln.Accept()
		if err != nil {
			logf("accept: %v", err)
			continue
		}
		go func() {
			ch := make(chan []byte, 256)
			mu.Lock()
			clients[ch] = struct{}{}
			mu.Unlock()
			defer func() {
				mu.Lock()
				delete(clients, ch)
				mu.Unlock()
				conn.Close()
			}()
			for line := range ch {
				if _, err := conn.Write(line); err != nil {
					return
				}
			}
		}()
	}
}

func readAccel() {
	// ALooper is per thread, so the whole sensor loop stays on one OS thread.
	runtime.LockOSThread()
	pkg := C.CString("dev.tomo.sensord")
	sm := C.ASensorManager_getInstanceForPackage(pkg)
	C.free(unsafe.Pointer(pkg))
	s := C.ASensorManager_getDefaultSensor(sm, C.ASENSOR_TYPE_ACCELEROMETER)
	if s == nil {
		logf("no accelerometer")
		return
	}
	lp := C.ALooper_prepare(C.ALOOPER_PREPARE_ALLOW_NON_CALLBACKS)
	q := C.ASensorManager_createEventQueue(sm, lp, 1, nil, nil)
	if C.ASensorEventQueue_registerSensor(q, s, 10000, 0) < 0 {
		logf("register failed")
		return
	}
	logf("accelerometer registered")
	var ev [16]C.ASensorEvent
	for {
		C.ALooper_pollOnce(-1, nil, nil, nil)
		for {
			n := C.ASensorEventQueue_getEvents(q, &ev[0], 16)
			if n <= 0 {
				break
			}
			for i := 0; i < int(n); i++ {
				d := (*[3]C.float)(unsafe.Pointer(&ev[i].anon0))
				broadcast(fmt.Appendf(nil, "{\"t\":%d,\"v\":[%g,%g,%g]}\n", int64(ev[i].timestamp), d[0], d[1], d[2]))
			}
		}
	}
}

var startOnce sync.Once

//export Java_dev_tomo_sensord_Core_start
func Java_dev_tomo_sensord_Core_start(env *C.JNIEnv, cls C.jclass) {
	startOnce.Do(func() {
		go serve()
		go readAccel()
	})
}

func main() {}

// Command libsensord is the native core of the sensord app, built with
// -buildmode=c-shared and loaded by the Java service through JNI. It serves
// the sensord protocol on proto.DefaultAddr.
package main

/*
#cgo LDFLAGS: -llog
#include <jni.h>
#include <stdlib.h>
#include <android/log.h>

static void logi(const char *msg) { __android_log_write(ANDROID_LOG_INFO, "sensord", msg); }
*/
import "C"

import (
	"fmt"
	"net"
	"sync"
	"unsafe"

	"github.com/TomoBossi/sensord/internal/hub"
	"github.com/TomoBossi/sensord/internal/ndk"
	"github.com/TomoBossi/sensord/internal/server"
	"github.com/TomoBossi/sensord/proto"
)

func logf(format string, args ...any) {
	s := C.CString(fmt.Sprintf(format, args...))
	C.logi(s)
	C.free(unsafe.Pointer(s))
}

func run() {
	be, err := ndk.Open("dev.tomo.sensord")
	if err != nil {
		logf("sensors: %v", err)
		return
	}
	h := hub.New(be)
	be.Dispatch = h.Dispatch
	logf("%d sensors", len(be.Sensors()))

	// Unix sockets are not an option: SELinux gives every app its own MLS
	// categories, which blocks connecting across apps (Termux -> us).
	ln, err := net.Listen("tcp", proto.DefaultAddr)
	if err != nil {
		logf("listen: %v", err)
		return
	}
	logf("listening on %s", proto.DefaultAddr)
	logf("serve: %v", (&server.Server{Hub: h, Logf: logf}).Serve(ln))
}

var startOnce sync.Once

//export Java_dev_tomo_sensord_Core_start
func Java_dev_tomo_sensord_Core_start(env *C.JNIEnv, cls C.jclass) {
	startOnce.Do(func() { go run() })
}

func main() {}

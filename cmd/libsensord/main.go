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
static jstring new_string(JNIEnv *env, const char *s) { return (*env)->NewStringUTF(env, s); }

// Calls from Go back into Java: Core.onAwake(boolean).
static JavaVM *jvm;
static jclass core;
static jmethodID on_awake;

static void jni_init(JNIEnv *env, jclass cls) {
	(*env)->GetJavaVM(env, &jvm);
	core = (*env)->NewGlobalRef(env, cls);
	on_awake = (*env)->GetStaticMethodID(env, core, "onAwake", "(Z)V");
}

static void jni_on_awake(int on) {
	JNIEnv *env;
	int attached = 0;
	if ((*jvm)->GetEnv(jvm, (void **)&env, JNI_VERSION_1_6) == JNI_EDETACHED) {
		(*jvm)->AttachCurrentThread(jvm, &env, NULL);
		attached = 1;
	}
	(*env)->CallStaticVoidMethod(env, core, on_awake, (jboolean)on);
	if ((*env)->ExceptionCheck(env)) {
		(*env)->ExceptionClear(env);
	}
	if (attached) {
		(*jvm)->DetachCurrentThread(jvm);
	}
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"net"
	"runtime"
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

var (
	mu      sync.Mutex
	srv     *server.Server
	failure string // why the core is not serving, if it isn't
)

func fail(format string, args ...any) {
	mu.Lock()
	failure = fmt.Sprintf(format, args...)
	mu.Unlock()
	logf("%s", failure)
}

func run() {
	be, err := ndk.Open("dev.tomo.sensord")
	if err != nil {
		fail("sensors: %v", err)
		return
	}
	h := hub.New(be)
	be.Dispatch = h.Dispatch
	h.OnAwake = func(on bool) {
		// JNI attach/detach must happen on one OS thread.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		v := C.int(0)
		if on {
			v = 1
		}
		C.jni_on_awake(v)
		logf("wake lock %v", on)
	}
	logf("%d sensors", len(be.Sensors()))

	// Unix sockets are not an option: SELinux gives every app its own MLS
	// categories, which blocks connecting across apps (Termux -> us).
	ln, err := net.Listen("tcp", proto.DefaultAddr)
	if err != nil {
		fail("listen: %v", err)
		return
	}
	logf("listening on %s", proto.DefaultAddr)
	s := &server.Server{Hub: h, Logf: logf}
	mu.Lock()
	srv = s
	mu.Unlock()
	err = s.Serve(ln)
	mu.Lock()
	srv = nil
	mu.Unlock()
	fail("serve: %v", err)
}

// statusJSON is what the app's status screen shows.
func statusJSON() []byte {
	mu.Lock()
	s, f := srv, failure
	mu.Unlock()
	out := struct {
		Addr  string `json:"addr"`
		Error string `json:"error,omitempty"`
		*proto.Status
	}{Addr: proto.DefaultAddr, Error: f}
	if s != nil {
		st := s.Status()
		out.Status = &st
	} else if f == "" {
		out.Error = "starting"
	}
	b, _ := json.Marshal(out)
	return b
}

//export Java_dev_tomo_sensord_Core_status
func Java_dev_tomo_sensord_Core_status(env *C.JNIEnv, cls C.jclass) C.jstring {
	cs := C.CString(string(statusJSON()))
	defer C.free(unsafe.Pointer(cs))
	return C.new_string(env, cs)
}

var startOnce sync.Once

//export Java_dev_tomo_sensord_Core_start
func Java_dev_tomo_sensord_Core_start(env *C.JNIEnv, cls C.jclass) {
	startOnce.Do(func() {
		C.jni_init(env, cls)
		go run()
	})
}

func main() {}

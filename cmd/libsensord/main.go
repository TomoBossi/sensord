// Command libsensord is the native core of the sensord app, built with
// -buildmode=c-shared and loaded by the Java service through JNI. It serves
// the sensord protocol on proto.DefaultAddr.
package main

/*
#cgo LDFLAGS: -llog
#include <jni.h>
#include <stdlib.h>
#include <string.h>
#include <android/log.h>

static void logi(const char *msg) { __android_log_write(ANDROID_LOG_INFO, "sensord", msg); }
static jstring new_string(JNIEnv *env, const char *s) { return (*env)->NewStringUTF(env, s); }

// Calls from Go back into Java (static methods of Core).
static JavaVM *jvm;
static jclass core;
static jmethodID on_awake, loc_start, loc_stop;

static void jni_init(JNIEnv *env, jclass cls) {
	(*env)->GetJavaVM(env, &jvm);
	core = (*env)->NewGlobalRef(env, cls);
	on_awake = (*env)->GetStaticMethodID(env, core, "onAwake", "(Z)V");
	loc_start = (*env)->GetStaticMethodID(env, core, "virtualStart", "(IJ)Ljava/lang/String;");
	loc_stop = (*env)->GetStaticMethodID(env, core, "virtualStop", "(I)V");
}

// env_get attaches the calling thread if needed; env_put undoes it.
static JNIEnv *env_get(int *attached) {
	JNIEnv *env;
	*attached = 0;
	if ((*jvm)->GetEnv(jvm, (void **)&env, JNI_VERSION_1_6) == JNI_EDETACHED) {
		(*jvm)->AttachCurrentThread(jvm, &env, NULL);
		*attached = 1;
	}
	return env;
}

static void env_put(JNIEnv *env, int attached) {
	if ((*env)->ExceptionCheck(env)) {
		(*env)->ExceptionClear(env);
	}
	if (attached) {
		(*jvm)->DetachCurrentThread(jvm);
	}
}

static void jni_on_awake(int on) {
	int a;
	JNIEnv *env = env_get(&a);
	(*env)->CallStaticVoidMethod(env, core, on_awake, (jboolean)on);
	env_put(env, a);
}

// jni_location_start returns NULL on success, or an error message to free().
static char *jni_location_start(int id, long long interval_ms) {
	int a;
	JNIEnv *env = env_get(&a);
	char *out = NULL;
	jstring r = (jstring)(*env)->CallStaticObjectMethod(env, core, loc_start, (jint)id, (jlong)interval_ms);
	if ((*env)->ExceptionCheck(env)) {
		out = strdup("java exception in virtualStart");
	} else if (r != NULL) {
		const char *c = (*env)->GetStringUTFChars(env, r, NULL);
		out = strdup(c);
		(*env)->ReleaseStringUTFChars(env, r, c);
		(*env)->DeleteLocalRef(env, r);
	}
	env_put(env, a);
	return out;
}

// copy_doubles copies up to max values of a Java double[] into out.
static int copy_doubles(JNIEnv *env, jdoubleArray a, double *out, int max) {
	jsize n = (*env)->GetArrayLength(env, a);
	if (n > max) {
		n = max;
	}
	(*env)->GetDoubleArrayRegion(env, a, 0, n, out);
	return n;
}

static void jni_location_stop(int id) {
	int a;
	JNIEnv *env = env_get(&a);
	(*env)->CallStaticVoidMethod(env, core, loc_stop, (jint)id);
	env_put(env, a);
}
*/
import "C"

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
	theHub  *hub.Hub // for location fixes arriving from Java
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
	nb, err := ndk.Open("io.github.tomobossi.sensord")
	if err != nil {
		fail("sensors: %v", err)
		return
	}
	be := &backend{Backend: nb}
	h := hub.New(be)
	nb.Dispatch = h.Dispatch
	mu.Lock()
	theHub = h
	mu.Unlock()
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

// Virtual sensors served by Java. The handles sit far above ASensor handles;
// the low bits are the Java-side id.
const (
	locationHandle = 0x40000001 // Locations id 1: fused provider
	gpsHandle      = 0x40000002 // Locations id 2: raw GNSS
	displayHandle  = 0x40000003 // Displays: screen rotation
	virtualBase    = 0x40000000
)

var locationSensors = []hub.Info{
	{Handle: locationHandle, Name: "location", Vendor: "android fused", Type: "location", TypeID: -1,
		MinDelayUs: 1000000, Mode: hub.Continuous, Wakeup: true, Default: true, Precise: true},
	{Handle: gpsHandle, Name: "gps", Vendor: "android gnss", Type: "gps", TypeID: -2,
		MinDelayUs: 1000000, Mode: hub.Continuous, Wakeup: true, Default: true, Precise: true},
	{Handle: displayHandle, Name: "display_rotation", Vendor: "android display", Type: "display_rotation", TypeID: -3,
		Mode: hub.OnChange, Wakeup: true, Default: true},
}

// backend is the NDK sensors plus the location virtual sensors, which are
// served by Java's LocationManager. Location is marked wakeup: fixes arrive
// by callback, so it needs no wake lock.
type backend struct {
	*ndk.Backend
}

func (b *backend) Sensors() []hub.Info {
	return append(b.Backend.Sensors(), locationSensors...)
}

func locationID(h int32) (int, bool) {
	switch h {
	case locationHandle, gpsHandle, displayHandle:
		return int(h - virtualBase), true
	}
	return 0, false
}

func (b *backend) Enable(h int32, periodUs int32) error {
	id, ok := locationID(h)
	if !ok {
		return b.Backend.Enable(h, periodUs)
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if msg := C.jni_location_start(C.int(id), C.longlong(max(periodUs/1000, 100))); msg != nil {
		defer C.free(unsafe.Pointer(msg))
		return errors.New(C.GoString(msg))
	}
	return nil
}

func (b *backend) SetPeriod(h int32, periodUs int32) error {
	if _, ok := locationID(h); !ok {
		return b.Backend.SetPeriod(h, periodUs)
	}
	return b.Enable(h, periodUs) // Java restarts updates at the new interval
}

func (b *backend) Disable(h int32) error {
	id, ok := locationID(h)
	if !ok {
		return b.Backend.Disable(h)
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	C.jni_location_stop(C.int(id))
	return nil
}

//export Java_io_github_tomobossi_sensord_Core_onVirtual
func Java_io_github_tomobossi_sensord_Core_onVirtual(env *C.JNIEnv, cls C.jclass, id C.jint, t C.jlong, values C.jdoubleArray) {
	mu.Lock()
	h := theHub
	mu.Unlock()
	if h == nil {
		return
	}
	var buf [16]C.double
	n := int(C.copy_doubles(env, values, &buf[0], C.int(len(buf))))
	v := make([]float64, n)
	for i := range v {
		v[i] = float64(buf[i])
		if math.IsInf(v[i], 0) {
			v[i] = math.NaN()
		}
	}
	h.Dispatch(int32(virtualBase+int32(id)), int64(t), v)
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

//export Java_io_github_tomobossi_sensord_Core_status
func Java_io_github_tomobossi_sensord_Core_status(env *C.JNIEnv, cls C.jclass) C.jstring {
	cs := C.CString(string(statusJSON()))
	defer C.free(unsafe.Pointer(cs))
	return C.new_string(env, cs)
}

var startOnce sync.Once

//export Java_io_github_tomobossi_sensord_Core_start
func Java_io_github_tomobossi_sensord_Core_start(env *C.JNIEnv, cls C.jclass) {
	startOnce.Do(func() {
		C.jni_init(env, cls)
		go run()
	})
}

func main() {}

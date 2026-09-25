package dev.tomo.sensord;

import android.os.PowerManager;

/** JNI bridge to the Go core (libsensord.so). */
final class Core {
    static {
        System.loadLibrary("sensord");
    }

    /** Starts the socket server and sensor loop; idempotent. */
    static native void start();

    /** Status snapshot as JSON: addr, error, connections, active, dropped. */
    static native String status();

    /** Held while a non-wakeup sensor is on; set by SensorService before start(). */
    static volatile PowerManager.WakeLock wakeLock;

    /**
     * Called by the Go core when the first non-wakeup sensor is enabled (true)
     * and when the last one is disabled (false). Without the lock the CPU
     * sleeps with the screen off and those sensors lose events.
     */
    static void onAwake(boolean on) {
        PowerManager.WakeLock wl = wakeLock;
        if (wl == null) {
            return;
        }
        if (on) {
            wl.acquire();
        } else if (wl.isHeld()) {
            wl.release();
        }
    }

    static boolean isAwake() {
        PowerManager.WakeLock wl = wakeLock;
        return wl != null && wl.isHeld();
    }

    private Core() {}
}

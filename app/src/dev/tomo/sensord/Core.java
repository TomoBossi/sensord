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

    /** A location fix for virtual sensor id (see Locations); missing fields are NaN. */
    static native void onLocation(int id, long elapsedNanos, double lat, double lon,
            double accuracy, double altitude, double speed, double bearing);

    /** Called by the Go core to start location updates; returns an error or null. */
    static String locationStart(int id, long intervalMs) {
        return Locations.start(id, intervalMs);
    }

    /** Called by the Go core to stop location updates. */
    static void locationStop(int id) {
        Locations.stop(id);
    }

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

package io.github.tomobossi.sensord;

import android.hardware.display.DisplayManager;
import android.os.Handler;
import android.os.Looper;
import android.os.SystemClock;
import android.view.Display;

/**
 * The "display_rotation" virtual sensor: how the screen content is rotated
 * relative to the phone's natural orientation, in degrees (0, 90, 180, 270),
 * reported when it changes. Clients need it to map sensor axes (fixed to the
 * phone's body) onto the screen when an app such as Termux rotates.
 */
final class Displays {
    static final int ID = 3;

    private static DisplayManager.DisplayListener listener;
    private static int last = -1;

    private Displays() {}

    private static void report(DisplayManager dm) {
        Display d = dm.getDisplay(Display.DEFAULT_DISPLAY);
        if (d == null) {
            return;
        }
        int deg = d.getRotation() * 90;
        if (deg != last) {
            last = deg;
            Core.onVirtual(ID, SystemClock.elapsedRealtimeNanos(), new double[]{deg});
        }
    }

    static synchronized String start() {
        SensorService s = SensorService.instance;
        if (s == null) {
            return "service not running";
        }
        stop();
        DisplayManager dm = s.getSystemService(DisplayManager.class);
        Handler main = new Handler(Looper.getMainLooper());
        listener = new DisplayManager.DisplayListener() {
            @Override public void onDisplayAdded(int id) {}
            @Override public void onDisplayRemoved(int id) {}
            @Override public void onDisplayChanged(int id) {
                if (id == Display.DEFAULT_DISPLAY) {
                    report(dm);
                }
            }
        };
        dm.registerDisplayListener(listener, main);
        // Posted, not called: start() runs inside the core's hub lock.
        main.post(() -> {
            last = -1; // a new subscriber always gets the current value
            report(dm);
        });
        return null;
    }

    static synchronized void stop() {
        SensorService s = SensorService.instance;
        if (s != null && listener != null) {
            s.getSystemService(DisplayManager.class).unregisterDisplayListener(listener);
        }
        listener = null;
    }
}

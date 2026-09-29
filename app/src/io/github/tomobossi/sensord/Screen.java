package io.github.tomobossi.sensord;

import android.database.ContentObserver;
import android.hardware.display.DisplayManager;
import android.provider.Settings;
import android.view.Display;

/**
 * The "screen" virtual sensor, on change: the display state (Android's
 * Display.STATE_* codes: 1 off, 2 on, 3 doze, 4 doze suspended, 6 on
 * suspended), the brightness setting (0 to 1) and automatic brightness (0/1).
 */
final class Screen extends Virtual {
    static final int ID = 7;

    private DisplayManager.DisplayListener listener;
    private ContentObserver observer;
    private double[] last;

    Screen() {
        super(ID);
    }

    private void report(SensorService s) {
        Display d = s.getSystemService(DisplayManager.class).getDisplay(Display.DEFAULT_DISPLAY);
        if (d == null) {
            return;
        }
        double[] v = {
                d.getState(),
                Settings.System.getInt(s.getContentResolver(), Settings.System.SCREEN_BRIGHTNESS, -1) / 255.0,
                Settings.System.getInt(s.getContentResolver(), Settings.System.SCREEN_BRIGHTNESS_MODE, 0),
        };
        if (v[1] < 0) {
            v[1] = Double.NaN;
        }
        synchronized (this) {
            if (last != null && java.util.Arrays.equals(v, last)) {
                return;
            }
            last = v;
        }
        emit(v);
    }

    @Override
    synchronized String start(SensorService s, long intervalMs) {
        stop(s);
        last = null; // a new subscriber always gets the current value
        listener = new DisplayManager.DisplayListener() {
            @Override public void onDisplayAdded(int id) {}
            @Override public void onDisplayRemoved(int id) {}
            @Override public void onDisplayChanged(int id) {
                if (id == Display.DEFAULT_DISPLAY) {
                    report(s);
                }
            }
        };
        s.getSystemService(DisplayManager.class).registerDisplayListener(listener, main);
        observer = new ContentObserver(main) {
            @Override
            public void onChange(boolean self) {
                report(s);
            }
        };
        s.getContentResolver().registerContentObserver(Settings.System.getUriFor(Settings.System.SCREEN_BRIGHTNESS), false, observer);
        s.getContentResolver().registerContentObserver(Settings.System.getUriFor(Settings.System.SCREEN_BRIGHTNESS_MODE), false, observer);
        main.post(() -> report(s));
        return null;
    }

    @Override
    synchronized void stop(SensorService s) {
        if (listener != null) {
            s.getSystemService(DisplayManager.class).unregisterDisplayListener(listener);
            listener = null;
        }
        if (observer != null) {
            s.getContentResolver().unregisterContentObserver(observer);
            observer = null;
        }
    }
}

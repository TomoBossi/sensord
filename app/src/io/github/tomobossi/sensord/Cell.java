package io.github.tomobossi.sensord;

import android.os.Build;
import android.telephony.CellSignalStrength;
import android.telephony.SignalStrength;
import android.telephony.TelephonyCallback;
import android.telephony.TelephonyManager;

/**
 * The "cell" virtual sensor, on change: the mobile signal level (0 to 4, as
 * the status bar shows it) and strength (dBm; NaN when unknown).
 */
final class Cell extends Virtual {
    static final int ID = 9;

    private TelephonyCallback cb;

    Cell() {
        super(ID);
    }

    private void report(SignalStrength ss) {
        if (ss == null) {
            return;
        }
        double dbm = Double.NaN;
        for (CellSignalStrength c : ss.getCellSignalStrengths()) {
            int d = c.getDbm();
            if (d != CellSignalStrength.SIGNAL_STRENGTH_NONE_OR_UNKNOWN && d != Integer.MAX_VALUE) {
                dbm = d;
                break;
            }
        }
        emit(ss.getLevel(), dbm);
    }

    @Override
    synchronized String start(SensorService s, long intervalMs) {
        if (Build.VERSION.SDK_INT < 31) {
            return "cell needs Android 12";
        }
        stop(s);
        TelephonyManager tm = s.getSystemService(TelephonyManager.class);
        class Listener extends TelephonyCallback implements TelephonyCallback.SignalStrengthsListener {
            @Override
            public void onSignalStrengthsChanged(SignalStrength ss) {
                report(ss);
            }
        }
        cb = new Listener();
        tm.registerTelephonyCallback(main::post, cb);
        main.post(() -> report(tm.getSignalStrength()));
        return null;
    }

    @Override
    synchronized void stop(SensorService s) {
        if (cb != null && Build.VERSION.SDK_INT >= 31) {
            s.getSystemService(TelephonyManager.class).unregisterTelephonyCallback(cb);
            cb = null;
        }
    }
}

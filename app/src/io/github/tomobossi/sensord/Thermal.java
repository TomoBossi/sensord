package io.github.tomobossi.sensord;

import android.os.Build;
import android.os.PowerManager;

/**
 * The "thermal" virtual sensor, polled: Android's thermal status (0 none,
 * 1 light, 2 moderate, 3 severe, 4 critical, 5 emergency, 6 shutdown) and
 * the thermal headroom forecast 10 s ahead (1.0 is where severe throttling
 * starts; NaN when unsupported).
 */
final class Thermal extends Virtual.Polled {
    static final int ID = 5;

    Thermal() {
        super(ID, 1000); // Android rate-limits the headroom to about 1 Hz
    }

    @Override
    double[] sample(SensorService s) {
        PowerManager pm = s.getSystemService(PowerManager.class);
        float head = Build.VERSION.SDK_INT >= 30 ? pm.getThermalHeadroom(10) : Float.NaN;
        return new double[]{pm.getCurrentThermalStatus(), head};
    }
}

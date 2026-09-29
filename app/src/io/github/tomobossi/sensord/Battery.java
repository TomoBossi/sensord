package io.github.tomobossi.sensord;

import android.content.Intent;
import android.content.IntentFilter;
import android.os.BatteryManager;
import android.os.PowerManager;

/**
 * The "battery" virtual sensor, polled: level (%), status, plugged, health
 * (Android's BatteryManager codes), temperature (°C), voltage (V), current
 * and average current (mA, positive while charging), charge (mAh), charge
 * cycles, time to full (s) and battery saver (0/1). What the device doesn't
 * report is NaN.
 */
final class Battery extends Virtual.Polled {
    static final int ID = 4;

    Battery() {
        super(ID, 250);
    }

    @Override
    double[] sample(SensorService s) {
        // The sticky broadcast holds the latest state; reading it registers nothing.
        Intent i = s.registerReceiver(null, new IntentFilter(Intent.ACTION_BATTERY_CHANGED));
        if (i == null) {
            return null;
        }
        BatteryManager bm = s.getSystemService(BatteryManager.class);
        int level = i.getIntExtra(BatteryManager.EXTRA_LEVEL, -1);
        int scale = i.getIntExtra(BatteryManager.EXTRA_SCALE, 100);
        int status = i.getIntExtra(BatteryManager.EXTRA_STATUS, BatteryManager.BATTERY_STATUS_UNKNOWN);
        int plugged = i.getIntExtra(BatteryManager.EXTRA_PLUGGED, 0);
        int temp = i.getIntExtra(BatteryManager.EXTRA_TEMPERATURE, Integer.MIN_VALUE);
        int mv = i.getIntExtra(BatteryManager.EXTRA_VOLTAGE, Integer.MIN_VALUE);
        int cycles = i.getIntExtra("android.os.extra.CYCLE_COUNT", -1); // BatteryManager.EXTRA_CYCLE_COUNT, API 34
        long toFull = bm.computeChargeTimeRemaining();
        return new double[]{
                level >= 0 && scale > 0 ? 100.0 * level / scale : Double.NaN,
                status,
                plugged,
                i.getIntExtra(BatteryManager.EXTRA_HEALTH, BatteryManager.BATTERY_HEALTH_UNKNOWN),
                temp != Integer.MIN_VALUE ? temp / 10.0 : Double.NaN,
                mv > 0 ? mv / 1000.0 : Double.NaN,
                current(bm, BatteryManager.BATTERY_PROPERTY_CURRENT_NOW, plugged),
                current(bm, BatteryManager.BATTERY_PROPERTY_CURRENT_AVERAGE, plugged),
                micro(bm.getIntProperty(BatteryManager.BATTERY_PROPERTY_CHARGE_COUNTER)),
                cycles >= 0 ? cycles : Double.NaN,
                toFull >= 0 ? toFull / 1000.0 : Double.NaN,
                s.getSystemService(PowerManager.class).isPowerSaveMode() ? 1 : 0,
        };
    }

    /** A property in µ-units as milli-units; NaN when unsupported. */
    private static double micro(int v) {
        return v == Integer.MIN_VALUE || v == 0 ? Double.NaN : v / 1000.0;
    }

    /**
     * A current in mA, positive while charging. Android documents that sign,
     * but vendors disagree, so it follows the plug: unplugged always drains.
     */
    private static double current(BatteryManager bm, int prop, int plugged) {
        int ua = bm.getIntProperty(prop);
        if (ua == Integer.MIN_VALUE) {
            return Double.NaN;
        }
        double ma = ua / 1000.0;
        if (plugged == 0 && ma > 0) {
            ma = -ma;
        }
        return ma;
    }
}

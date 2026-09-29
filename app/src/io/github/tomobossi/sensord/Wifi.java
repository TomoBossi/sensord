package io.github.tomobossi.sensord;

import android.net.wifi.WifiInfo;
import android.net.wifi.WifiManager;

/**
 * The "wifi" virtual sensor, polled: the connected network's signal (dBm),
 * its level (0 to 4, as the status bar shows it), link speed (Mbps) and
 * frequency (MHz). NaN, with level 0, while not connected.
 */
final class Wifi extends Virtual.Polled {
    static final int ID = 8;

    Wifi() {
        super(ID, 1000);
    }

    @Override
    @SuppressWarnings("deprecation") // getConnectionInfo is still the simplest way to poll the RSSI
    double[] sample(SensorService s) {
        WifiManager wm = s.getApplicationContext().getSystemService(WifiManager.class);
        WifiInfo i = wm.getConnectionInfo();
        if (i == null || i.getRssi() <= -127 || i.getLinkSpeed() <= 0) {
            return new double[]{Double.NaN, 0, Double.NaN, Double.NaN};
        }
        int rssi = i.getRssi();
        int level = Math.min(4, WifiManager.calculateSignalLevel(rssi, 5));
        return new double[]{rssi, level, i.getLinkSpeed(), i.getFrequency()};
    }
}

package dev.tomo.sensord;

import android.Manifest;
import android.content.pm.PackageManager;
import android.content.pm.ServiceInfo;
import android.hardware.GeomagneticField;
import android.location.Location;
import android.location.LocationListener;
import android.location.LocationManager;
import android.os.Build;
import android.os.Handler;
import android.os.Looper;

/**
 * Location as virtual sensors for the Go core: id 1 is "location" (Android's
 * fused provider: satellites, Wi-Fi and cell towers), id 2 is "gps" (raw
 * satellite fixes). The core calls start/stop like it enables sensors, and
 * fixes go back through Core.onVirtual.
 */
final class Locations {
    static final int FUSED = 1, GPS = 2;

    private static final LocationListener[] listeners = new LocationListener[3];
    private static boolean locationTyped; // service runs with the location FGS type

    private Locations() {}

    private static String provider(LocationManager lm, int id) {
        if (id == FUSED && Build.VERSION.SDK_INT >= 31 && lm.hasProvider(LocationManager.FUSED_PROVIDER)) {
            return LocationManager.FUSED_PROVIDER;
        }
        return LocationManager.GPS_PROVIDER;
    }

    /** Starts updates every intervalMs; returns an error message, or null. */
    static synchronized String start(int id, long intervalMs) {
        SensorService s = SensorService.instance;
        if (s == null) {
            return "service not running";
        }
        if (s.checkSelfPermission(Manifest.permission.ACCESS_FINE_LOCATION) != PackageManager.PERMISSION_GRANTED) {
            return "location permission not granted: allow it on the sensord screen "
                    + "(am start -n dev.tomo.sensord/.MainActivity)";
        }
        // A foreground service needs the location type to use location, and
        // Android only allows adding it while the app may use location: when
        // started from the foreground, or with "Allow all the time".
        if (!locationTyped) {
            try {
                s.retype(ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE | ServiceInfo.FOREGROUND_SERVICE_TYPE_LOCATION);
                locationTyped = true;
            } catch (RuntimeException e) {
                return "Android refused location for a background service (" + e.getMessage()
                        + "); open the sensord screen once, or set location to \"Allow all the time\"";
            }
        }
        stop(id);
        LocationManager lm = s.getSystemService(LocationManager.class);
        String p = provider(lm, id);
        if (!lm.isProviderEnabled(p)) {
            return "location is turned off on the phone (" + p + " provider disabled)";
        }
        LocationListener l = loc -> deliver(id, loc);
        listeners[id] = l;
        try {
            lm.requestLocationUpdates(p, intervalMs, 0f, l, Looper.getMainLooper());
            // Hand out the last known fix at once, like on-change sensors do,
            // so a client doesn't wait for the next fix (minutes, indoors).
            // Posted, not called: start() runs inside the core's hub lock, and
            // delivering from here would re-enter it.
            Location last = lm.getLastKnownLocation(p);
            if (last != null) {
                new Handler(Looper.getMainLooper()).post(() -> deliver(id, last));
            }
        } catch (SecurityException e) {
            listeners[id] = null;
            return "location: " + e.getMessage();
        }
        return null;
    }

    static synchronized void stop(int id) {
        SensorService s = SensorService.instance;
        LocationListener l = listeners[id];
        if (s != null && l != null) {
            s.getSystemService(LocationManager.class).removeUpdates(l);
        }
        listeners[id] = null;
    }

    private static void deliver(int id, Location l) {
        // From Android's model of Earth's field at this place: the magnetic
        // declination (degrees, east positive; add it to a magnetic heading
        // for true north) and the field strength a compass should measure.
        GeomagneticField g = new GeomagneticField((float) l.getLatitude(), (float) l.getLongitude(),
                l.hasAltitude() ? (float) l.getAltitude() : 0f, System.currentTimeMillis());
        Core.onVirtual(id, l.getElapsedRealtimeNanos(), new double[]{l.getLatitude(), l.getLongitude(),
                l.hasAccuracy() ? l.getAccuracy() : Double.NaN,
                l.hasAltitude() ? l.getAltitude() : Double.NaN,
                l.hasSpeed() ? l.getSpeed() : Double.NaN,
                l.hasBearing() ? l.getBearing() : Double.NaN,
                g.getDeclination(),
                g.getFieldStrength() / 1000.0}); // expected field here, uT
    }
}

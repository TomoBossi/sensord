package io.github.tomobossi.sensord;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.content.Intent;
import android.content.pm.ServiceInfo;
import android.os.IBinder;
import android.os.PowerManager;

/** Foreground service that keeps the process, and so its sensor access, alive. */
public class SensorService extends Service {
    private static final String CHANNEL = "sensord";

    /** The running service, for Locations; null when not running. */
    static volatile SensorService instance;

    private Notification notification;
    private int types = ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE;

    /** Re-declares the foreground service types; throws if Android refuses. */
    void retype(int newTypes) {
        startForeground(1, notification, newTypes);
        types = newTypes;
    }

    @Override
    public void onDestroy() {
        instance = null;
        super.onDestroy();
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        instance = this;
        NotificationManager nm = getSystemService(NotificationManager.class);
        nm.createNotificationChannel(new NotificationChannel(CHANNEL, "sensord", NotificationManager.IMPORTANCE_MIN));
        notification = new Notification.Builder(this, CHANNEL)
                .setSmallIcon(R.drawable.ic_stat)
                .setContentTitle("sensord running")
                .setContentIntent(PendingIntent.getActivity(this, 0,
                        new Intent(this, MainActivity.class), PendingIntent.FLAG_IMMUTABLE))
                .setOngoing(true)
                .build();
        startForeground(1, notification, types);
        if (Core.wakeLock == null) {
            PowerManager.WakeLock wl = getSystemService(PowerManager.class)
                    .newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "sensord:sensors");
            wl.setReferenceCounted(false);
            Core.wakeLock = wl;
        }
        Core.start();
        return START_STICKY;
    }

    @Override
    public IBinder onBind(Intent intent) {
        return null;
    }
}

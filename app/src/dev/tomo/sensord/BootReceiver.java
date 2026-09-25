package dev.tomo.sensord;

import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;

/**
 * Starts the service at boot and after the app is updated. Both broadcasts are
 * exempt from Android's background foreground-service start restrictions.
 */
public class BootReceiver extends BroadcastReceiver {
    @Override
    public void onReceive(Context context, Intent intent) {
        String a = intent.getAction();
        if (Intent.ACTION_BOOT_COMPLETED.equals(a) || Intent.ACTION_MY_PACKAGE_REPLACED.equals(a)) {
            context.startForegroundService(new Intent(context, SensorService.class));
        }
    }
}

package dev.tomo.sensord;

import android.app.Activity;
import android.content.Intent;
import android.os.Bundle;

/** Starts the service and gets out of the way. */
public class MainActivity extends Activity {
    @Override
    protected void onCreate(Bundle b) {
        super.onCreate(b);
        startForegroundService(new Intent(this, SensorService.class));
        finish();
    }
}

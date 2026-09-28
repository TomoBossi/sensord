package io.github.tomobossi.sensord;

import android.Manifest;
import android.app.Activity;
import android.content.ComponentName;
import android.content.Intent;
import android.content.pm.PackageManager;
import android.graphics.Typeface;
import android.net.Uri;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.os.PowerManager;
import android.os.Process;
import android.provider.Settings;
import android.view.View;
import android.widget.Button;
import android.widget.LinearLayout;
import android.widget.ScrollView;
import android.widget.Switch;
import android.widget.TextView;

import org.json.JSONArray;
import org.json.JSONObject;

/** Status screen: what the core is doing, plus the launcher-icon and battery settings. */
public class MainActivity extends Activity {
    private static final long REFRESH_MS = 1000;

    private final Handler handler = new Handler(Looper.getMainLooper());
    private final Runnable refresh = new Runnable() {
        @Override
        public void run() {
            render();
            handler.postDelayed(this, REFRESH_MS);
        }
    };

    private TextView status;
    private Button battery;
    private Button steps;
    private Button location;
    private Switch icon;

    @Override
    protected void onCreate(Bundle b) {
        super.onCreate(b);
        startForegroundService(new Intent(this, SensorService.class));

        LinearLayout root = new LinearLayout(this);
        root.setOrientation(LinearLayout.VERTICAL);
        int pad = dp(20);
        root.setPadding(pad, pad, pad, pad);

        TextView title = new TextView(this);
        title.setText("sensord");
        title.setTextSize(28);
        root.addView(title);

        status = new TextView(this);
        status.setTypeface(Typeface.MONOSPACE);
        status.setTextSize(13);
        status.setPadding(0, dp(12), 0, dp(20));
        root.addView(status);

        icon = new Switch(this);
        icon.setText("Show launcher icon");
        icon.setChecked(isIconShown());
        icon.setOnCheckedChangeListener((v, on) -> setIconShown(on));
        root.addView(icon);

        battery = new Button(this);
        battery.setOnClickListener(v -> requestBatteryExemption());
        root.addView(battery);

        steps = new Button(this);
        steps.setOnClickListener(v -> requestPermissions(new String[]{Manifest.permission.ACTIVITY_RECOGNITION}, 1));
        root.addView(steps);

        location = new Button(this);
        location.setOnClickListener(v -> {
            if (checkSelfPermission(Manifest.permission.ACCESS_FINE_LOCATION) != PackageManager.PERMISSION_GRANTED) {
                requestPermissions(new String[]{Manifest.permission.ACCESS_FINE_LOCATION,
                        Manifest.permission.ACCESS_COARSE_LOCATION}, 2);
            } else {
                // "Allow all the time" can only be chosen in the app's settings.
                startActivity(new Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS,
                        Uri.parse("package:" + getPackageName())));
            }
        });
        root.addView(location);

        Button stop = new Button(this);
        stop.setText("Stop sensord");
        stop.setOnClickListener(v -> {
            stopService(new Intent(this, SensorService.class));
            finishAndRemoveTask();
            // The Go core has no shutdown path; ending the process releases
            // every sensor and closes every connection.
            Process.killProcess(Process.myPid());
        });
        root.addView(stop);

        TextView hint = new TextView(this);
        hint.setTextSize(12);
        hint.setPadding(0, dp(20), 0, 0);
        hint.setText("With the icon hidden, open this screen from Termux:\n"
                + "am start -n io.github.tomobossi.sensord/.MainActivity\n\n"
                + "Starts automatically at boot and after updates.");
        root.addView(hint);

        ScrollView scroll = new ScrollView(this);
        scroll.addView(root);
        setContentView(scroll);
    }

    @Override
    protected void onResume() {
        super.onResume();
        handler.post(refresh);
    }

    @Override
    protected void onPause() {
        super.onPause();
        handler.removeCallbacks(refresh);
    }

    private void render() {
        StringBuilder sb = new StringBuilder();
        try {
            JSONObject s = new JSONObject(Core.status());
            String err = s.optString("error", "");
            if (!err.isEmpty()) {
                sb.append("Not serving: ").append(err).append('\n');
            } else {
                sb.append("Serving on ").append(s.getString("addr")).append('\n');
                sb.append("Connections: ").append(s.getInt("connections")).append('\n');
                sb.append("Keeping CPU awake: ").append(Core.isAwake() ? "yes" : "no").append('\n');
                long dropped = s.getLong("dropped");
                if (dropped > 0) {
                    sb.append("Dropped events: ").append(dropped).append('\n');
                }
                JSONArray active = s.getJSONArray("active");
                sb.append('\n');
                if (active.length() == 0) {
                    sb.append("No sensors powered: nobody is reading.\n");
                } else {
                    sb.append("Powered sensors:\n");
                    for (int i = 0; i < active.length(); i++) {
                        JSONObject a = active.getJSONObject(i);
                        sb.append("  ").append(a.getString("name"));
                        double hz = a.getDouble("hz");
                        sb.append(hz > 0 ? String.format("  %.0f Hz", hz) : "  " + a.optString("mode"));
                        double measured = a.optDouble("measured_hz", 0);
                        if (measured > 0) {
                            sb.append(String.format(" (actual %.1f)", measured));
                        }
                        int subs = a.getInt("subscribers");
                        sb.append(", ").append(subs).append(subs == 1 ? " reader" : " readers").append('\n');
                    }
                }
            }
        } catch (Exception e) {
            sb.append("Status unavailable: ").append(e.getMessage());
        }
        status.setText(sb);

        boolean exempt = getSystemService(PowerManager.class).isIgnoringBatteryOptimizations(getPackageName());
        battery.setText(exempt ? "Battery optimization: exempt" : "Exempt from battery optimization");
        battery.setEnabled(!exempt);

        boolean stepsOk = checkSelfPermission(Manifest.permission.ACTIVITY_RECOGNITION) == PackageManager.PERMISSION_GRANTED;
        steps.setText(stepsOk ? "Step sensors: allowed" : "Allow step sensors (physical activity)");
        steps.setEnabled(!stepsOk);

        boolean fine = checkSelfPermission(Manifest.permission.ACCESS_FINE_LOCATION) == PackageManager.PERMISSION_GRANTED;
        boolean always = checkSelfPermission(Manifest.permission.ACCESS_BACKGROUND_LOCATION) == PackageManager.PERMISSION_GRANTED;
        location.setText(!fine ? "Allow location"
                : always ? "Location: allowed all the time"
                : "Location: while in use (tap for \"all the time\")");
        location.setEnabled(!always);
    }

    private ComponentName launcherAlias() {
        return new ComponentName(this, getPackageName() + ".Launcher");
    }

    private boolean isIconShown() {
        int state = getPackageManager().getComponentEnabledSetting(launcherAlias());
        return state != PackageManager.COMPONENT_ENABLED_STATE_DISABLED;
    }

    private void setIconShown(boolean shown) {
        getPackageManager().setComponentEnabledSetting(launcherAlias(),
                shown ? PackageManager.COMPONENT_ENABLED_STATE_ENABLED : PackageManager.COMPONENT_ENABLED_STATE_DISABLED,
                PackageManager.DONT_KILL_APP);
    }

    private void requestBatteryExemption() {
        Intent i = new Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS,
                Uri.parse("package:" + getPackageName()));
        startActivity(i);
    }

    private int dp(int v) {
        return Math.round(v * getResources().getDisplayMetrics().density);
    }
}

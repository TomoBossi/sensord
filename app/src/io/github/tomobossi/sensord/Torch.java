package io.github.tomobossi.sensord;

import android.hardware.camera2.CameraManager;

import java.util.HashSet;
import java.util.Set;

/** The "flashlight" virtual sensor: 1 while any flashlight is on, else 0, on change. */
final class Torch extends Virtual {
    static final int ID = 6;

    private CameraManager.TorchCallback cb;
    private final Set<String> on = new HashSet<>();
    private int last = -1;

    Torch() {
        super(ID);
    }

    @Override
    synchronized String start(SensorService s, long intervalMs) {
        stop(s);
        on.clear();
        last = -1; // a new subscriber always gets the current value
        cb = new CameraManager.TorchCallback() {
            @Override
            public void onTorchModeChanged(String camera, boolean enabled) {
                synchronized (Torch.this) {
                    if (enabled) {
                        on.add(camera);
                    } else {
                        on.remove(camera);
                    }
                    int v = on.isEmpty() ? 0 : 1;
                    if (v != last) {
                        last = v;
                        emit(v);
                    }
                }
            }
        };
        // Android calls back at once with every camera's current state.
        s.getSystemService(CameraManager.class).registerTorchCallback(cb, main);
        return null;
    }

    @Override
    synchronized void stop(SensorService s) {
        if (cb != null) {
            s.getSystemService(CameraManager.class).unregisterTorchCallback(cb);
            cb = null;
        }
    }
}

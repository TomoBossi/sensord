package io.github.tomobossi.sensord;

import android.os.Handler;
import android.os.Looper;
import android.os.SystemClock;

/**
 * A virtual sensor served from Java, for the sources that are not ASensors
 * (battery, thermal state, radios...). The Go core starts and stops it like a
 * sensor; readings go back through Core.onVirtual.
 */
abstract class Virtual {
    final int id;
    final Handler main = new Handler(Looper.getMainLooper());

    Virtual(int id) {
        this.id = id;
    }

    /**
     * Starts the source; returns an error message, or null. Runs inside the
     * core's hub lock, so readings must be posted, never emitted inline.
     */
    abstract String start(SensorService s, long intervalMs);

    abstract void stop(SensorService s);

    void emit(double... v) {
        Core.onVirtual(id, SystemClock.elapsedRealtimeNanos(), v);
    }

    /** A source read by polling, every interval but no faster than minMs. */
    abstract static class Polled extends Virtual {
        private final long minMs;
        private volatile Runnable tick;

        Polled(int id, long minMs) {
            super(id);
            this.minMs = minMs;
        }

        /** One reading, or null to skip this round. */
        abstract double[] sample(SensorService s);

        @Override
        synchronized String start(SensorService s, long intervalMs) {
            stop(s);
            long every = Math.max(intervalMs, minMs);
            tick = new Runnable() {
                @Override
                public void run() {
                    if (tick != this) {
                        return; // stopped, or restarted at another interval
                    }
                    double[] v = sample(s);
                    if (v != null) {
                        emit(v);
                    }
                    main.postDelayed(this, every);
                }
            };
            main.post(tick);
            return null;
        }

        @Override
        synchronized void stop(SensorService s) {
            if (tick != null) {
                main.removeCallbacks(tick);
                tick = null;
            }
        }
    }
}

package dev.tomo.sensord;

/** JNI bridge to the Go core (libsensord.so). */
final class Core {
    static {
        System.loadLibrary("sensord");
    }

    /** Starts the socket server and sensor loop; idempotent. */
    static native void start();

    private Core() {}
}

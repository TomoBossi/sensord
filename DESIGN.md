# sensord — design

An Android app that lets any local process (Python, Go or shell in Termux, or
another app) stream phone sensor data at a rate of its choosing. It only powers
the sensors someone is reading.

## Why an APK (findings, 2026-09-24)

- **Termux:API's `termux-sensor` is unusable here:** a single global session,
  always registered at `SENSOR_DELAY_UI` (~16 Hz), and `-d` only re-prints the
  latest snapshot.
- **A native NDK daemon inside Termux works while the screen is on:** 99 Hz at
  100 Hz requested, 394 Hz at 400 Hz. But delivery **stops 60 s after screen
  off**: Termux is not a real foreground service on Android 15
  (`isForeground=false`), so its uid goes idle and SensorService cuts
  continuous sensors. The battery whitelist, `RUN_ANY_IN_BACKGROUND` and the
  exempted standby bucket were all already set, so none of them help.
- **Android does not reduce the rate per client:** two listeners at 10 Hz and
  100 Hz both got ~99 Hz. Whoever sits in the middle must downsample.

So the app holds a real foreground service, owns every sensor registration and
multiplexes to clients.

## Components

```
 Termux process ──┐                         ┌─ SensorManager
 Termux process ──┼── abstract unix socket ─┤    (one registration per sensor,
 other app ───────┘      @sensord           │     at the fastest rate needed)
                                            └─ per-client downsampling
```

- **SensorService** (foreground service, type `specialUse`): accepts
  connections, runs the hub. Started at boot and by opening the app.
- **BootReceiver**: `BOOT_COMPLETED` → start the service.
- **MainActivity**: status (clients, active sensors, rates), a "hide launcher
  icon" toggle, and a battery-exemption shortcut. When hidden, it can still be
  opened with `am start -n dev.tomo.sensord/.MainActivity`.
- **Launcher entry** is an `activity-alias`, so hiding it means disabling the
  alias via `setComponentEnabledSetting`, the same approach as Termux:API.
- **Clients**: a Python module plus a `sensord` CLI in Termux. Other languages
  just open the socket, since the protocol is plain text.

## Transport

The socket is a unix socket in the **abstract namespace**, `@sensord`. The
app's data dir is not reachable from Termux, and an abstract socket needs no
file. From Python: `socket.connect("\0sensord")`; from Go: `"@sensord"`; from
the shell: `socat - ABSTRACT-CONNECT:sensord`.

**Access control:** any app can reach an abstract socket, so the server checks
the peer uid (`SO_PEERCRED`) against an allow list. Default: its own uid and
Termux's uid (resolved via `PackageManager`, currently 10261).

## Protocol

Newline-delimited JSON, one object per line, both directions. At 400 Hz that
comes to roughly 40 KB/s per stream, which is negligible. A binary framing can
be added later as a per-subscription option if it ever matters.

Client → server (`id` is chosen by the client and echoed back):

```json
{"op":"list"}
{"op":"sub","id":1,"sensor":"accelerometer","hz":50}
{"op":"sub","id":2,"sensor":"CHOP_CHOP"}
{"op":"unsub","id":1}
{"op":"get","sensor":"light"}
{"op":"ping"}
```

Server → client:

```json
{"op":"list","sensors":[{"name":"bmi3xy_acc","type":"accelerometer","type_id":1,"max_hz":400,"mode":"continuous","wakeup":false}, ...]}
{"op":"ok","id":1,"hz":50}
{"id":1,"t":863308967866138,"v":[-0.298,0.065,9.963],"acc":3}
{"op":"err","id":2,"msg":"unknown sensor"}
```

- `sensor` accepts a type name (`accelerometer`, `gyroscope`, `light`, … →
  the default sensor of that type) or an exact sensor name for vendor and
  non-default sensors.
- `t` is the event timestamp in ns (the `elapsedRealtimeNanos` clock). `v` is
  the raw values array and `acc` is accuracy.
- `hz` omitted or `0` means as fast as the sensor goes.
- `get` returns the latest value, for programs that prefer polling. If the
  sensor is off, `get` turns it on and waits for the first sample. It stays on
  while gets keep arriving and switches off 2 s after the last one, so a
  polling loop keeps it warm by itself.

## Rates and energy

- Each sensor is registered **only while at least one subscription or recent
  `get` needs it**. A dropped connection, even a crashed or killed client, is
  seen as EOF, and its subscriptions are removed at once.
- Each sensor is registered at the **fastest period any subscription asks
  for**, clamped to the sensor's `minDelay`. It is re-registered when that
  changes.
- **Per-subscription downsampling by timestamp:** emit an event when
  `t >= due`, then `due += period`, resyncing to `t + period` if more than one
  period behind. A 10 Hz client sharing a 100 Hz sensor gets every 10th sample
  on average, with no drift.
- Downsampling rules by reporting mode:
  - on-change sensors (light, step counter): every change is forwarded, with
    `hz` acting as a cap;
  - one-shot sensors (significant motion): delivered once, then the
    subscription ends.
- A **partial wake lock is held only while at least one sensor is active**.
  Without it the CPU suspends with the screen off and non-wakeup sensor events
  stall; with zero subscriptions, nothing is held.
- `HIGH_SAMPLING_RATE_SENSORS` is requested so rates above 200 Hz are allowed.

## Staying invisible

- **Notification:** foreground services must post one. The channel uses
  `IMPORTANCE_MIN`, and the app does not request `POST_NOTIFICATIONS`, so on
  Android 13+ it shows only in the quick-settings "active apps" list.
- **Launcher icon:** toggled from MainActivity; see Components.
- The **battery optimization exemption** is requested once from
  MainActivity.

## Build

No Gradle and no AndroidX. `build.sh` runs aapt2 → javac → d8 → zip →
apksigner against `~/.local/share/android-sdk/platforms/android-35/android.jar`.
`./build.sh install` also runs `adb install -r`.

The signing keystore lives outside the repo, at
`~/.local/share/android-keys/sensord.jks`. **If it is lost, updates require an
uninstall.** It needs a backup somewhere once remotes exist.

The downsampler and hub are plain Java with no Android imports, so they are
unit tested on the desktop JVM.

## Layout

```
app/AndroidManifest.xml
app/src/dev/tomo/sensord/…     Java sources
app/res/…                      minimal resources
clients/python/sensord.py      client module + CLI
test/…                         JVM unit tests
build.sh
```

## Milestones

1. Service + socket + `list`/`sub`/`unsub`, Python client. Verify the
   screen-off case with the same 75 s test that failed before.
2. Downsampling, `get`, wake lock, per-uid access control.
3. Boot start, hidden icon toggle, status screen.
4. Polish: CLI, one-shot sensors, docs.

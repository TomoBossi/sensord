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
multiplexes to clients. The spike (commit 5d68296) confirmed it: 99 Hz through
138 s of screen off, with no gaps.

## Components

```
 Termux process ──┐                          ┌─ NDK ASensorManager
 Termux process ──┼── TCP 127.0.0.1:47474 ───┤    (one registration per sensor,
 other app ───────┘   (Go core, in the APK)  │     at the fastest rate needed)
                                             └─ per-client downsampling
```

- **Go core** (`cmd/libsensord`, built with cgo as a c-shared `libsensord.so`):
  the socket server, protocol, hub, downsampling and the sensor reading itself,
  via the NDK sensor API. Java calls into it through a single JNI entry point.
- **SensorService** (Java, foreground service, type `specialUse`): keeps the
  process in the foreground state and loads the Go core. Started by the first
  client, by opening the app, or at boot where the device allows it.
- **BootReceiver**: `BOOT_COMPLETED` → start the service.
- **MainActivity**: status (clients, active sensors, rates), a "hide launcher
  icon" toggle, and a battery-exemption shortcut. When hidden, it can still be
  opened with `am start -n io.github.tomobossi.sensord/.MainActivity`.
- **Launcher entry** is an `activity-alias`, so hiding it means disabling the
  alias via `setComponentEnabledSetting`, the same approach as Termux:API.
- **Clients**: a Go package plus a `sensord` CLI in Termux, and a Python
  module. Other languages just open the socket, since the protocol is plain
  text.
- **Replay** (`sensord replay`, CLI side): the same hub and server as the
  app, over a backend that plays a recording instead of the NDK. One
  playback clock drives every sensor, so they stay in step however late each
  is subscribed; timestamps become playback time, increasing across loops.

## Transport

The transport is **TCP on `127.0.0.1:47474`**. A unix socket was the first
choice, but SELinux gives each app its own MLS categories (sensord
`untrusted_app:c7,…`, Termux `untrusted_app_27:c5,…`), which blocks connects
across apps. Termux:API escapes this only by sharing Termux's uid, which
requires Termux's signing key. Loopback is not reachable from off the device.

**Access control:** none for now. Any local app with `INTERNET` can connect,
but ordinary sensors need no permission, so any app could read them directly
anyway. The only thing it would gain is our foreground status while it is
itself in the background. Revisit if body sensors are ever added.

## Protocol

Newline-delimited JSON, one object per line, both directions. At 400 Hz that
comes to roughly 40 KB/s per stream, which is negligible. The types live in
package `proto`.

Client → server. `id` is optional and chosen by the client; the server echoes
it in the reply. For `sub` it is required, and it also tags every event of
that stream.

```json
{"op":"list","id":1}
{"op":"sub","id":2,"sensor":"accelerometer","hz":50}
{"op":"sub","id":3,"sensor":"CHOP_CHP"}
{"op":"unsub","id":4,"sub":2}
{"op":"ping","id":5}
```

Server → client:

```json
{"op":"list","id":1,"sensors":[{"name":"bmi3xy_acc","vendor":"bmi","type":"accelerometer","type_id":1,"max_hz":400,"mode":"continuous","default":true}, ...]}
{"op":"ok","id":2,"sensor":"bmi3xy_acc","hz":50}
{"id":2,"t":863308967866138,"v":[-0.298,0.065,9.963]}
{"op":"err","id":3,"msg":"unknown sensor"}
{"op":"pong","id":5}
```

- Event lines are the only messages without `op`.
- `sensor` accepts an exact sensor name, or a type (`accelerometer`,
  `gyroscope`, `light`, …) meaning the default sensor of that type. Matching
  is case-insensitive.
- `t` is the event timestamp in ns, on the `elapsedRealtimeNanos` clock.
- `v` is the values array per the Android `SensorEvent` docs. For vendor types
  with an unknown layout, trailing zeros are trimmed.
- `hz` omitted or `0` means every event. The reply says what was granted,
  clamped to the sensor's maximum.
- Events of a subscription always arrive before the `ok` for its `unsub`.
- A client that falls about 1024 lines behind has events dropped, never
  replies.
- On-change sensors deliver their cached last value right after `sub`, with
  its original, possibly old, timestamp.

`get` returns the latest reading for programs that prefer polling:

```json
{"op":"get","id":6,"sensor":"light"}
{"op":"value","id":6,"sensor":"ltr569_l","t":899500397744320,"v":[52]}
```

If the sensor is off, `get` powers it on (at `hz`, default up to 50 Hz) and
waits for the first reading, for at most 5 s. It stays on for 2 s after the
last `get`, so a polling loop keeps it warm by itself. Measured on the device
via the CLI: about 150 ms cold and 75 ms warm, most of it process startup.

One-shot sensors (significant motion, the Motorola gestures such as
`CHOP_CHOP`) can be subscribed. Android disables them after each trigger;
the hub re-arms them after 100 ms for as long as someone is subscribed, so a
subscription is a stream of triggers. `get` refuses them.

## Rates and energy

- Each sensor is registered **only while at least one subscription or recent
  `get` needs it**. A dropped connection, even a crashed or killed client, is
  seen as EOF, and its subscriptions are removed at once.
- Each sensor is registered at the **fastest period any subscription asks
  for**, clamped to the sensor's `minDelay`. It is re-registered when that
  changes.
- **Per-subscription downsampling by timestamp:** emit an event when
  `t >= due - tol`, then `due += period`, resyncing if more than one period
  behind. `tol` is half the *measured* source interval: hardware drifts from
  the registered period, and may clamp it (the accelerometer here never goes
  below 12.5 Hz). A 10 Hz client sharing a 100 Hz sensor gets every 10th sample
  on average, with no drift.
- Downsampling rules by reporting mode:
  - on-change sensors (light, step counter): every change is forwarded, with
    `hz` acting as a cap;
  - one-shot sensors: every trigger is forwarded, then re-armed.
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

No Gradle and no AndroidX. `build.sh` runs go (c-shared) → aapt2 → javac →
d8 → zip → apksigner against `~/.local/share/android-sdk/platforms/android-35/android.jar`.
`./build.sh install` also runs `adb install -r`.

The signing keystore never enters the repo. `build.sh` reads it from
`$SENSORD_KEYSTORE` (default `~/.local/share/android-keys/sensord.jks`) and
creates one there if missing. **If it is lost, updates require an
uninstall**, so it must be backed up off the phone.

The hub and downsampler are pure Go with no cgo, so they are tested with
plain `go test` in Termux.

## Layout

```
cmd/libsensord/          Go core entry point (c-shared, JNI export)
internal/ndk/            NDK sensor backend (cgo)
internal/hub/            subscriptions, rates, downsampling (pure Go)
internal/server/         socket server and protocol handling
proto/                   protocol types
cmd/sensord/             Termux CLI, including record and replay
client/                  Go client package
clients/python/          Python client
app/AndroidManifest.xml
app/src/io/github/tomobossi/sensord/  Java shell
build.sh
```

## Milestones

1. ~~Spike: Go core in the APK streams the accelerometer; screen-off test.~~
   Done.
2. ~~Hub: `list`/`sub`/`unsub`, per-client downsampling, Go client + CLI.~~
   Done.
3. ~~`get`, wake lock (test with Termux's own wake lock released).~~ Done.
4. ~~Boot start, hidden icon toggle, status screen.~~ Done; the icon toggle
   is verified (hidden with no placeholder). Boot start is blocked by
   DuraSpeed on the test phone, so clients auto-start the app instead (see
   Device notes).
5. ~~Polish: Python client, one-shot sensors, get, app icon.~~ Done; every
   gesture sensor (CHOP_CHOP, FLIP_TWIST, FLIP, WAKE_GESTURE, TILT_DETECTOR,
   SIGNIFICANT_MOTION) fires repeatedly through re-arm on the device.
6. ~~Location, display rotation, magnetometer calibration, record/replay.~~
   Done.

## Location

`location` (fused provider) and `gps` (GNSS) are virtual sensors with handles
0x40000001/2, above any ASensor handle. `cmd/libsensord` wraps the NDK backend
in a composite that routes those handles to Java's `Locations` over JNI:
enable becomes `requestLocationUpdates` with the registration period as the
interval (capped at 1 s), disable becomes `removeUpdates`. Fixes come back
through `Core.onLocation` into `hub.Dispatch`, so rates, sharing, `get` and
linger all work unchanged. They are marked wakeup (callbacks need no wake
lock) and `Precise` (float64 output). The last known fix is posted to the main
thread, not delivered inline, because enable runs under the hub lock.

On first use the service re-declares its foreground types as
`specialUse|location`. Android allows that only while the app may use
location: started from the foreground (auto-start from Termux qualifies) or
with background location granted. A refusal becomes the client's error.

## Phone state

`battery`, `thermal`, `flashlight`, `screen`, `wifi` and `cell` are more
virtual sensors (ids 4 to 9), each a `Virtual` on the Java side. Sources
Android pushes (torch callback, display listener, telephony callback) are
on-change and wakeup. Sources that have to be polled (battery current,
thermal headroom, Wi-Fi RSSI) are continuous with a minimum period, and not
wakeup, so the hub holds the wake lock while they are subscribed and a
Handler loop polls them at the registered period.

Missing values are NaN end to end: Java sends NaN, the wire carries `null`,
and `proto.Values` decodes `null` back to NaN.

## Device notes (moto g17 power)

- **Boot start does not work here.** MediaTek's DuraSpeed (a preinstalled,
  hidden background-app limiter) skips BOOT_COMPLETED for this app:
  `skipped by policy at enqueue: ... suppress to start process of
  staticReceiver`. Its settings screen is protected by a signature permission
  and has no entry in Settings. **Instead, clients start the app on demand:**
  on a refused connection to the default address, the Go and Python clients
  run `am startservice -n io.github.tomobossi.sensord/.SensorService` (Termux's `am`
  has no `start-foreground-service`) and retry for 10 s; about 2.5 s from a
  dead app to the first reading. `SENSORD_NO_AUTOSTART=1` disables this.
  SensorService is exported for that reason. A persisted JobScheduler job
  also got past DuraSpeed in a forced test, but was dropped as unnecessary.

- **Step detector timestamps** occasionally run a few hundred ms late, so two
  steps arrive ~20 ms apart right after an unusually long gap. They are real
  steps: the step counter counts both (tested 2026-09-25, batch-by-batch
  agreement), and sensord passes them through unchanged. Don't filter them.
- **Step counter** needs ~10 steps before it counts, then adds them at once;
  it matched 31 for 30 steps walked with the phone in a pocket.
- **Wake lock is required.** With the screen off and no wake lock held
  anywhere, a 50 Hz accelerometer stream lost data in gaps of up to 97 s
  (sensor-time gaps, so dropped, not delayed). sensord now holds a partial
  wake lock (`sensord:sensors`) exactly while a non-wakeup sensor is
  enabled; wakeup sensors (gestures, step detector) don't take it.
- **Accelerometer** never runs below 12.5 Hz; slower subscriptions are
  downsampled.

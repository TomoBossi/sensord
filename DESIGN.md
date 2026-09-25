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
  process in the foreground state and loads the Go core. Started at boot and by
  opening the app.
- **BootReceiver**: `BOOT_COMPLETED` → start the service.
- **MainActivity**: status (clients, active sensors, rates), a "hide launcher
  icon" toggle, and a battery-exemption shortcut. When hidden, it can still be
  opened with `am start -n dev.tomo.sensord/.MainActivity`.
- **Launcher entry** is an `activity-alias`, so hiding it means disabling the
  alias via `setComponentEnabledSetting`, the same approach as Termux:API.
- **Clients**: a Go package plus a `sensord` CLI in Termux, and a Python
  module. Other languages just open the socket, since the protocol is plain
  text.

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

The signing keystore lives outside the repo, at
`~/.local/share/android-keys/sensord.jks`. **If it is lost, updates require an
uninstall.** It needs a backup somewhere once remotes exist.

The hub and downsampler are pure Go with no cgo, so they are tested with
plain `go test` in Termux.

## Layout

```
cmd/libsensord/          Go core entry point (c-shared, JNI export, NDK glue)
internal/hub/            subscriptions, rates, downsampling (pure Go)
cmd/sensord/             Termux CLI
client/                  Go client package
clients/python/          Python client
app/AndroidManifest.xml
app/src/dev/tomo/sensord/  Java shell
build.sh
```

## Milestones

1. ~~Spike: Go core in the APK streams the accelerometer; screen-off test.~~
   Done.
2. ~~Hub: `list`/`sub`/`unsub`, per-client downsampling, Go client + CLI.~~
   Done.
3. `get`, wake lock (test with Termux's own wake lock released).
4. ~~Boot start, hidden icon toggle, status screen.~~ Done; boot start and
   the icon toggle verified (hidden with no placeholder); boot start still
   needs a reboot test.
5. Polish: Python client, one-shot sensors, docs.

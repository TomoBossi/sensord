# sensord

Read your phone's sensors from any program within [`Termux`](https://github.com/termux/termux-app), at a rate of your choice. An Android app holds the sensors and serves them over a local socket. Sensors are powered only while something is reading them, and the data keeps flowing even with the screen off.

## Quick start

Within `Termux`:

```sh
sensord list                       # list the available sensors on your phone
sensord get light                  # one reading, a timestamp and a value
sensord stream accelerometer 20    # many readings, 20 times a second
sensord help stream                # details for any command
```

If the app isn't already running, its first client starts it.

## CLI

| Command | What it does |
|---|---|
| `sensord list` | every sensor: name, type, max rate, reporting mode |
| `sensord status` | connected programs; which sensors are powered, at what rate |
| `sensord get SENSOR` | the current reading, once |
| `sensord stream [-n N] [-json] SENSOR [HZ]` | every event as it arrives |
| `sensord rate SENSOR [HZ]` | events per second, to check a rate |
| `sensord record [-o FILE] [-d DUR] SENSOR[@HZ]...` | record sensors to a file |
| `sensord replay [-addr ADDR] [-loop] [-speed X] FILE` | serve a recording as if it were the phone |

`sensord help COMMAND`, `sensord COMMAND -h` and `sensord COMMAND help` all show a command's details.

**SENSOR** is an exact name from `sensord list` (case-insensitive, such as `bmi3xy_gyro` or `CHOP_CHOP`), or a type (`accelerometer`, `gyroscope`, `light`, `step_counter`, …), which picks the default sensor of that type.

**HZ** is the rate you want, omit it or pass 0 for every event the sensor produces. The granted rate is printed to stderr, clamped to the sensor's maximum.

### Record and replay

`sensord record` writes every event of the sensors you name to a JSON-lines file (a header describing them, then `{"s":index,"t":ns,"v":[...]}` per
event). `sensord replay` serves that file on another port with the same protocol as the app, events at their recorded timing, each sensor starting
where the playback is when a program subscribes, so they stay in step. Clients reach it through `SENSORD_ADDR`, which the Go package, the Python
module and the CLI all honor, so any program runs against a recording unchanged, for testing with real motion:

```sh
sensord record -o walk.jsonl -d 1m gravity@60 rotation_vector@60 step_detector
sensord replay -loop walk.jsonl &
SENSORD_ADDR=127.0.0.1:47475 sensordemo compass
```

## Go

```go
import "github.com/TomoBossi/sensord/client"

c, err := client.Dial("") // starts the app if needed
if err != nil {
	log.Fatal(err)
}
defer c.Close()

sub, err := c.Subscribe("accelerometer", 50)
if err != nil {
	log.Fatal(err)
}
for ev := range sub.C { // ev.T: ns since boot, ev.V: values
	fmt.Println(ev.T, ev.V)
}
```

Also `c.Get(sensor, 0)`, `c.Sensors()`, `c.Status()`, and `sub.Close()`.

## Python

Installed in `Termux`'s Python (`pip install -e clients/python`), so any script can use it. Standard library only.

```python
import sensord

with sensord.connect() as c:
    print(c.get("light"))                     # Event(t=..., v=[49])
    with c.subscribe("accelerometer", 50) as sub:
        for ev in sub:
            print(ev.t, ev.v)
```

Also `c.sensors()`, `c.status()`, and `sub.next(timeout=...)`.

## Any other language

The protocol is newline-delimited JSON over TCP on `127.0.0.1:47474`, so any language with sockets works; even bash does:

```sh
exec 3<>/dev/tcp/127.0.0.1/47474
echo '{"op":"get","id":1,"sensor":"light"}' >&3
head -1 <&3    # {"op":"value","id":1,"sensor":"ltr569_l","t":7347745050583,"v":[49]}
```

Streaming: send `{"op":"sub","id":2,"sensor":"gyroscope","hz":100}`, then read lines like `{"id":2,"t":...,"v":[x,y,z]}` until you close the socket. Raw clients don't auto-start the app, start it with `am startservice -n io.github.tomobossi.sensord/.SensorService`.

## What the values mean

- **Timestamps** (`t`) are nanoseconds since boot, on the same clock as Android's `elapsedRealtimeNanos` and `CLOCK_BOOTTIME`. They are not wall time.
- **The magnetometer** adds a 4th value: its calibration status (0 unreliable, 1 low, 2 medium, 3 high; wave the phone in a figure 8 to raise it).
- **Values** (`v`) follow Android's [`SensorEvent`](https://developer.android.com/reference/android/hardware/SensorEvent#values) layout per type: accelerometer m/s², gyroscope rad/s, magnetometer µT, light lux, and so on.
- **On-change sensors** (light, step counter) only report changes. A new subscription first receives the last known value, with its original, possibly old, timestamp.
- **One-shot sensors** (gestures such as `CHOP_CHOP` and `FLIP_TWIST`, significant motion) produce one event per trigger. `sensord` re-arms them, so a subscription gets every trigger.
- **`step_detector`** emits `1` per step; count the events. `step_counter` is the total since boot and updates in batches of a few steps.

## Location

Two virtual sensors, used exactly like the others (`sensord get location`, `sensord stream gps 1`, `c.Subscribe("location", 1)`):

| Sensor | Source | Use it for |
|---|---|---|
| `location` | Android's fused provider: satellites, Wi-Fi and cell towers, balanced power | decent accuracy, works indoors |
| `gps` | raw satellite fixes (GNSS) | best accuracy outdoors, more power, nothing indoors |

Values: `[lat, lon, accuracy_m, altitude_m, speed_m/s, bearing_deg, declination_deg, field_uT]`, with full `float64` precision. The last two come from Android's model of Earth's magnetic field at that place: add the declination to a magnetic heading to get true north, and compare the magnetometer's magnitude with `field_uT` to detect local disturbances. Fields a fix lacks are `null` in streams (and 0 in `get`). The rate is capped at 1 Hz. A new subscription gets the last known fix at once, with its original timestamp, then live fixes.

Like the sensors, location is only requested from Android while a client is subscribed, and released when the last one leaves. Needs the location permission from the app screen. "Allow all the time" also lets it work when `sensord` was started without anything in the foreground. Heading (the direction where the phone points in map apps) comes from the `rotation_vector` sensor.

## Display rotation

`display_rotation` reports how the screen is rotated from the phone's natural (portrait) orientation: `0`, `90`, `180` or `270` degrees, whenever it changes (Android's `Display.getRotation`). Sensor axes are fixed to the phone's body, so an app that rotates, like `Termux` in landscape, needs this value to know which axis is "up" on screen: rotate sensor vectors by it around `z`. It costs nothing while unsubscribed.

## The app

It runs as a foreground service with no visible window. Its launcher icon can be hidden from the app drawer through the app itself. Its status screen can be opened from `Termux`:

```sh
am start -n io.github.tomobossi.sensord/.MainActivity
```

The screen shows powered sensors, rates and readers, and has switches for the launcher icon, the battery exemption and the step-sensor permission, plus a Stop button.

- **Power:** each sensor runs at the fastest rate anyone asked for, and only while someone reads it. A client that disconnects or crashes releases its sensors at once. A partial wake lock is held only while a non-wakeup sensor is on.
- **Boot:** on the Moto G17 Power, MediaTek's DuraSpeed blocks start at boot, so the app starts on first use instead. It restarts itself after updates.
- **Access:** loopback only, so nothing off the phone can connect. Any local app could, but ordinary sensors need no permissions anyway.

## Install

Build the app on the phone (see [Building](#building)), then install the CLI and whichever clients you need:

```sh
go install github.com/TomoBossi/sensord/cmd/sensord@latest     # the CLI
go get github.com/TomoBossi/sensord@latest                     # the Go package, in your module
pip install "git+https://github.com/TomoBossi/sensord#subdirectory=clients/python"
```

## Building

On the phone, in `Termux`, with no Gradle. Build dependencies:

```sh
pkg install golang clang ndk-sysroot openjdk-21 aapt2 d8 apksigner zip
```

`android.jar` from Android's SDK platform 35 is also needed (no `Termux` package ships it). `build.sh` looks for it at
`~/.local/share/android-sdk/platforms/android-35/android.jar`; set `ANDROID_JAR` to use another path.

```sh
./build.sh                                          # Go core + APK, in build/sensord.apk
./build.sh install                                  # the same, then adb install
go test ./...                                       # hub and server tests
```

### Signing key

The APK must be signed, and the key is never in this repo. `build.sh` uses `~/.local/share/android-keys/sensord.jks` (override with
`SENSORD_KEYSTORE`, and its password with `SENSORD_KEYSTORE_PASS`, default `sensord`). If none exists, it creates one there. To make
your own instead:

```sh
keytool -genkeypair -keystore sensord.jks -alias sensord -keyalg RSA -keysize 2048 -validity 10000
```

Android only accepts an update signed with the same key as the installed app, so **back the keystore up**. Losing it means uninstalling
the app before installing a new build.

## Troubleshooting

- **`sensord not reachable ... (is the app installed?)`**: auto-start failed. Check `adb shell pidof io.github.tomobossi.sensord`, or open the status screen.
- **`register STEP_COUNTER: error -22`**: the physical-activity permission isn't granted; grant it on the status screen.
- **`location permission not granted` / `Android refused location for a background service`**: allow location on the status screen, ideally "all the time".
- **A rate lower than requested**: check `sensord status` to see what the hardware delivers. Some sensors have a fixed floor or ceiling; the accelerometer never goes below 12.5 Hz, and slower requests are downsampled.
- **Events dropped**: a client that falls about 1024 lines behind loses events, so read promptly. The count shows in `sensord status`.

## License

MIT, see [LICENSE](LICENSE).

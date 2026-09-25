# sensord

Read this phone's sensors from any program in Termux, at the rate each
program chooses. An Android app holds the sensors and serves them over a
local socket. Sensors are powered only while something is reading them, and
the data keeps flowing with the screen off.

How it works inside: [DESIGN.md](DESIGN.md).

## Quick start

```sh
sensord list                       # the 27 sensors on this phone
sensord get light                  # one reading: TIMESTAMP LUX
sensord stream accelerometer 20    # x y z in m/s², 20 times a second
sensord help stream                # details for any command
```

Nothing needs to be started first. If the app isn't running, the first
client starts it and waits, which takes about 2.5 s once.

## CLI

| Command | What it does |
|---|---|
| `sensord list` | every sensor: name, type, max rate, reporting mode |
| `sensord status` | connected programs; which sensors are powered, at what rate |
| `sensord get SENSOR` | the current reading, once |
| `sensord stream [-n N] [-json] SENSOR [HZ]` | every event as it arrives |
| `sensord rate SENSOR [HZ]` | events per second, to check a rate |

`sensord help COMMAND`, `sensord COMMAND -h` and `sensord COMMAND help` all
show a command's details.

**SENSOR** is an exact name from `sensord list` (case-insensitive, such as
`bmi3xy_gyro` or `CHOP_CHOP`), or a type (`accelerometer`, `gyroscope`,
`light`, `step_counter`, …), which picks the default sensor of that type.

**HZ** is the rate you want; omit it or pass 0 for every event the sensor
produces. The granted rate is printed to stderr, clamped to the sensor's
maximum.

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

The repo has no remote, so point your module at the local copy:

```sh
go mod edit -require github.com/TomoBossi/sensord@v0.0.0 \
            -replace github.com/TomoBossi/sensord=$HOME/projects/sensord
```

## Python

Installed in Termux's Python (`pip install -e clients/python`), so any
script can use it. Standard library only.

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

The protocol is newline-delimited JSON over TCP on `127.0.0.1:47474`, so any
language with sockets works; even bash does:

```sh
exec 3<>/dev/tcp/127.0.0.1/47474
echo '{"op":"get","id":1,"sensor":"light"}' >&3
head -1 <&3    # {"op":"value","id":1,"sensor":"ltr569_l","t":7347745050583,"v":[49]}
```

Streaming: send `{"op":"sub","id":2,"sensor":"gyroscope","hz":100}`, then
read lines like `{"id":2,"t":...,"v":[x,y,z]}` until you close the socket.
All the ops are in [DESIGN.md](DESIGN.md#protocol). Raw clients don't
auto-start the app; start it with
`am startservice -n dev.tomo.sensord/.SensorService`.

## What the values mean

- **Timestamps** (`t`) are nanoseconds since boot, on the same clock as
  Android's `elapsedRealtimeNanos` and `CLOCK_BOOTTIME`. They are not wall
  time.
- **Values** (`v`) follow Android's
  [SensorEvent](https://developer.android.com/reference/android/hardware/SensorEvent#values)
  layout per type: accelerometer m/s², gyroscope rad/s, magnetometer µT,
  light lux, and so on.
- **On-change sensors** (light, step counter) only report changes. A new
  subscription first receives the last known value, with its original,
  possibly old, timestamp.
- **One-shot sensors** (gestures such as `CHOP_CHOP` and `FLIP_TWIST`,
  significant motion) produce one event per trigger. sensord re-arms them,
  so a subscription gets every trigger.
- **`step_detector`** emits `1` per step; count the events. `step_counter`
  is the total since boot and updates in batches of a few steps.
- **Step sensors** need the "physical activity" permission, granted once from
  the app screen.

## The app

It runs as a foreground service with no visible window. Its launcher icon is
hidden; open its status screen from Termux:

```sh
am start -n dev.tomo.sensord/.MainActivity
```

The screen shows powered sensors, rates and readers, and has switches for
the launcher icon, the battery exemption and the step-sensor permission, plus
a Stop button.

- **Power:** each sensor runs at the fastest rate anyone asked for, and only
  while someone reads it. A client that disconnects or crashes releases its
  sensors at once. A partial wake lock is held only while a non-wakeup
  sensor is on.
- **Boot:** on this phone, MediaTek's DuraSpeed blocks start at boot, so the
  app starts on first use instead (see above). It restarts itself after
  updates.
- **Access:** loopback only, so nothing off the phone can connect. Any local
  app could, but ordinary sensors need no permission anyway.

## Building

On the phone, no Gradle (setup in DESIGN.md):

```sh
./build.sh install                                  # Go core + APK, adb install
go build -o $PREFIX/bin/sensord ./cmd/sensord       # the CLI
go test ./...                                       # hub and server tests
```

The signing key is at `~/.local/share/android-keys/sensord.jks`, outside the
repo. Back it up: without it, an update means uninstalling first, which
drops the granted permissions.

## Troubleshooting

- **`sensord not reachable ... (is the app installed?)`**: auto-start
  failed. Check `adb shell pidof dev.tomo.sensord`, or open the status
  screen.
- **`register STEP_COUNTER: error -22`**: the physical-activity permission
  isn't granted; grant it on the status screen.
- **A rate lower than requested**: check `sensord status` to see what the
  hardware delivers. Some sensors have a fixed floor or ceiling; the
  accelerometer never goes below 12.5 Hz, and slower requests are
  downsampled.
- **Events dropped**: a client that falls about 1024 lines behind loses
  events, so read promptly. The count shows in `sensord status`.

// Command sensord is the Termux-side CLI for the sensord app. Run
// "sensord help" for the command list and "sensord help COMMAND" for details.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/TomoBossi/sensord/client"
)

type command struct {
	name    string
	args    string // synopsis after the command name
	summary string
	help    string // details for "sensord help NAME"
	flags   func(*flag.FlagSet)
	run     func(c *client.Client, fs *flag.FlagSet) error
	// noClient: the command doesn't talk to the app (run gets nil).
	noClient bool
}

const sensorHelp = `SENSOR is an exact name from "sensord list" (case-insensitive), or a type
such as accelerometer, gyroscope, light, step_counter, which picks the
default sensor of that type.`

var (
	streamN    *int
	streamJSON *bool
)

var commands = []command{
	{
		name:    "list",
		summary: "list the sensors on this device",
		help: `Prints every sensor with its type, maximum rate, reporting mode and flags.

Modes:
  continuous  reports at a steady rate (accelerometer, gyroscope, ...)
  on-change   reports only when the value changes (light, step_counter, ...)
  one-shot    fires on a trigger (gestures such as CHOP_CHOP, significant
              motion); sensord re-arms it, so a stream gets every trigger
  special     sensor-specific (step_detector: one event per step, whose
              value is always 1; count the lines, not the value. Its
              timestamps can run a few hundred ms late, so two real steps
              occasionally arrive ~20 ms apart)

Flags: "default" marks the sensor a type name resolves to; "wakeup" sensors
can wake the phone from sleep.`,
		run: func(c *client.Client, fs *flag.FlagSet) error { return list(c) },
	},
	{
		name:    "status",
		summary: "show connected programs and which sensors are powered",
		help: `Shows how many programs are connected (besides this command) and, for each
powered sensor, the requested rate, the rate it actually delivers, and how
many readers share it. "no sensors powered" means nothing is using power.`,
		run: func(c *client.Client, fs *flag.FlagSet) error { return status(c) },
	},
	{
		name:    "get",
		args:    "SENSOR",
		summary: "print the current reading once",
		help: `Prints one reading as "TIMESTAMP VALUE...". If nobody is reading the sensor,
it is powered on just long enough to get a reading, and stays on for 2 s so
repeated gets are fast. Useful for polling from scripts.

` + sensorHelp + `

Example:
  sensord get light          # e.g. 899500397744320 52  (lux)`,
		run: func(c *client.Client, fs *flag.FlagSet) error {
			if fs.NArg() != 1 {
				return errUsage
			}
			ev, err := c.Get(fs.Arg(0), 0)
			if err == nil {
				fmt.Println(formatEvent(ev))
			}
			return err
		},
	},
	{
		name:    "stream",
		args:    "[-n N] [-json] SENSOR [HZ]",
		summary: "print every event as it arrives",
		help: `Prints each event as "TIMESTAMP VALUE..." (or JSON with -json) until
interrupted. This is the data itself; to just see how many events arrive per
second, use "sensord rate".

` + sensorHelp + `

HZ is the delivery rate; omit it or pass 0 for every event the sensor
produces. The rate actually granted is printed to stderr first.

TIMESTAMP is nanoseconds since boot (Android's elapsedRealtimeNanos).

Examples:
  sensord stream accelerometer 20     # x y z in m/s^2, 20 times a second
  sensord stream -n 1 step_counter    # steps since boot, once
  sensord stream step_detector        # one line per step (value always 1)
  sensord stream CHOP_CHOP            # one line per chop gesture`,
		flags: func(fs *flag.FlagSet) {
			streamN = fs.Int("n", 0, "stop after N events (0 = forever)")
			streamJSON = fs.Bool("json", false, "print JSON lines")
		},
		run: func(c *client.Client, fs *flag.FlagSet) error { return stream(c, fs.Args()) },
	},
	{
		name:    "rate",
		args:    "SENSOR [HZ]",
		summary: "count events per second, without printing them",
		help: `Subscribes like "sensord stream" but prints only how many events arrived in
each second. Handy for checking that a sensor delivers the rate you asked for.

` + sensorHelp + `

Example:
  sensord rate gyroscope 200`,
		run: func(c *client.Client, fs *flag.FlagSet) error { return rate(c, fs.Args()) },
	},
}

var errUsage = fmt.Errorf("usage")

func find(name string) *command {
	for i := range commands {
		if commands[i].name == name {
			return &commands[i]
		}
	}
	return nil
}

func synopsis(c *command) string {
	return strings.TrimSpace("sensord " + c.name + " " + c.args)
}

func mainHelp() {
	width := termWidth()
	fmt.Println(reflow("sensord: read phone sensors through the sensord app", width) + "\n\nusage:")
	var rows [][2]string
	for _, c := range commands {
		rows = append(rows, [2]string{"  " + synopsis(&c), c.summary})
	}
	rows = append(rows, [2]string{"  sensord help [COMMAND]", "this help, or a command's details"})
	fmt.Print(columns(rows, width))
	fmt.Println()
	fmt.Println(reflow(`Every command also takes -h. If the app isn't running, it is started
automatically (set SENSORD_NO_AUTOSTART=1 to disable). Set SENSORD_ADDR to
reach a server other than 127.0.0.1:47474.`, width))
}

func cmdHelp(c *command) {
	fmt.Printf("usage: %s\n\n%s\n", synopsis(c), reflow(c.help, termWidth()))
	if c.flags != nil {
		fs := flag.NewFlagSet(c.name, flag.ContinueOnError)
		c.flags(fs)
		fmt.Println("\nflags:")
		fs.SetOutput(os.Stdout)
		fs.PrintDefaults()
	}
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		if len(args) >= 2 {
			if c := find(args[1]); c != nil {
				cmdHelp(c)
				return
			}
			fmt.Fprintf(os.Stderr, "unknown command %q\n\n", args[1])
		}
		mainHelp()
		if len(args) == 0 {
			os.Exit(2)
		}
		return
	}
	cmd := find(args[0])
	if cmd == nil {
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", args[0])
		mainHelp()
		os.Exit(2)
	}
	fs := flag.NewFlagSet(cmd.name, flag.ContinueOnError)
	fs.Usage = func() { cmdHelp(cmd) }
	if cmd.flags != nil {
		cmd.flags(fs)
	}
	rest := args[1:]
	if len(rest) > 0 && rest[0] == "help" {
		cmdHelp(cmd)
		return
	}
	if err := fs.Parse(rest); err != nil {
		if err == flag.ErrHelp {
			return
		}
		os.Exit(2)
	}

	var c *client.Client
	if !cmd.noClient {
		var err error
		if c, err = client.Dial(""); err != nil { // honors SENSORD_ADDR
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer c.Close()
	}
	if err := cmd.run(c, fs); err != nil {
		if err == errUsage {
			fmt.Fprintf(os.Stderr, "usage: %s\n(sensord help %s for details)\n", synopsis(cmd), cmd.name)
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func list(c *client.Client) error {
	sensors, err := c.Sensors()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tTYPE\tMAX HZ\tMODE\tFLAGS")
	for _, s := range sensors {
		var flags []string
		if s.Default {
			flags = append(flags, "default")
		}
		if s.Wakeup {
			flags = append(flags, "wakeup")
		}
		hz := "-"
		if s.MaxHz > 0 {
			hz = strconv.FormatFloat(s.MaxHz, 'f', 0, 64)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", s.Name, s.Type, hz, s.Mode, strings.Join(flags, ","))
	}
	return w.Flush()
}

func status(c *client.Client) error {
	st, err := c.Status()
	if err != nil {
		return err
	}
	// This connection counts too; don't report it.
	fmt.Printf("connections: %d (besides this one)\n", st.Connections-1)
	if st.Dropped > 0 {
		fmt.Printf("dropped events: %d\n", st.Dropped)
	}
	if len(st.Active) == 0 {
		fmt.Println("no sensors powered")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "POWERED\tHZ\tACTUAL\tREADERS")
	for _, a := range st.Active {
		hz := a.Mode
		if a.Hz > 0 {
			hz = strconv.FormatFloat(a.Hz, 'f', 1, 64)
		}
		fmt.Fprintf(w, "%s\t%s\t%.1f\t%d\n", a.Name, hz, a.MeasuredHz, a.Subscribers)
	}
	return w.Flush()
}

func subscribe(c *client.Client, args []string) (*client.Subscription, error) {
	if len(args) < 1 || len(args) > 2 {
		return nil, errUsage
	}
	hz := 0.0
	if len(args) == 2 {
		var err error
		if hz, err = strconv.ParseFloat(args[1], 64); err != nil {
			return nil, fmt.Errorf("bad HZ %q", args[1])
		}
	}
	sub, err := c.Subscribe(args[0], hz)
	if err != nil {
		return nil, err
	}
	granted := "every event"
	if sub.Hz > 0 {
		granted = strconv.FormatFloat(sub.Hz, 'f', -1, 64) + " Hz"
	}
	fmt.Fprintf(os.Stderr, "%s at %s\n", sub.Sensor, granted)
	return sub, nil
}

func stream(c *client.Client, args []string) error {
	n, asJSON := streamN, streamJSON
	sub, err := subscribe(c, args)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	count := 0
	for ev := range sub.C {
		if *asJSON {
			enc.Encode(map[string]any{"t": ev.T, "v": ev.V})
		} else {
			fmt.Println(formatEvent(ev))
		}
		if count++; *n > 0 && count >= *n {
			return nil
		}
	}
	return c.Err()
}

func formatEvent(ev client.Event) string {
	var b strings.Builder
	b.WriteString(strconv.FormatInt(ev.T, 10))
	for _, v := range ev.V {
		b.WriteByte(' ')
		b.WriteString(strconv.FormatFloat(v, 'g', -1, 64))
	}
	return b.String()
}

func rate(c *client.Client, args []string) error {
	sub, err := subscribe(c, args)
	if err != nil {
		return err
	}
	tk := time.NewTicker(time.Second)
	defer tk.Stop()
	count, secs := 0, 0
	for {
		select {
		case _, ok := <-sub.C:
			if !ok {
				return c.Err()
			}
			count++
		case <-tk.C:
			secs++
			fmt.Printf("%3ds %d\n", secs, count)
			count = 0
		}
	}
}

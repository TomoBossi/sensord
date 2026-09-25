// Command sensord is the Termux-side CLI for the sensord app.
//
//	sensord list                        sensors on this device
//	sensord status                      connections and powered sensors
//	sensord stream [-n N] [-json] SENSOR [HZ]
//	                                    print events; SENSOR is a name or a type
//	sensord rate SENSOR [HZ]            print delivered events per second
//	sensord get SENSOR                  print the latest reading once
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

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  sensord list
  sensord status
  sensord stream [-n N] [-json] SENSOR [HZ]
  sensord rate SENSOR [HZ]
  sensord get SENSOR

SENSOR is an exact name from "sensord list" or a type such as accelerometer,
gyroscope, light. HZ defaults to 0, meaning as fast as the sensor goes.
Set SENSORD_ADDR to reach a server other than 127.0.0.1:47474.
`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	c, err := client.Dial(os.Getenv("SENSORD_ADDR"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprintln(os.Stderr, "start it with: am start -n dev.tomo.sensord/.MainActivity")
		os.Exit(1)
	}
	defer c.Close()

	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "list":
		err = list(c)
	case "status":
		err = status(c)
	case "stream":
		err = stream(c, args)
	case "get":
		if len(args) != 1 {
			usage()
		}
		var ev client.Event
		if ev, err = c.Get(args[0], 0); err == nil {
			fmt.Println(formatEvent(ev))
		}
	case "rate":
		err = rate(c, args)
	default:
		usage()
	}
	if err != nil {
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
		usage()
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
	fs := flag.NewFlagSet("stream", flag.ExitOnError)
	n := fs.Int("n", 0, "stop after N events (0 = forever)")
	asJSON := fs.Bool("json", false, "print JSON lines")
	fs.Parse(args)
	sub, err := subscribe(c, fs.Args())
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
		b.WriteString(strconv.FormatFloat(v, 'g', 6, 64))
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

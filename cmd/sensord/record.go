package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TomoBossi/sensord/client"
	"github.com/TomoBossi/sensord/internal/hub"
	"github.com/TomoBossi/sensord/internal/server"
	"github.com/TomoBossi/sensord/proto"
)

// A recording is JSON lines: a header describing the recorded sensors,
// then one line per event, in arrival order:
//
//	{"sensord_recording":1,"sensors":[{"name":"...",...},...]}
//	{"s":0,"t":123456789,"v":[0.1,9.8,0.3]}
//
// s indexes the header's sensors; t is the event's timestamp (ns since
// boot) and v its values, as delivered.
type recHeader struct {
	Version int            `json:"sensord_recording"`
	Sensors []proto.Sensor `json:"sensors"`
}

type recEvent struct {
	S int       `json:"s"`
	T int64     `json:"t"`
	V []float64 `json:"v"`
}

var (
	recOut   *string
	recDur   *time.Duration
	playAddr *string
	playLoop *bool
	playRate *float64
)

func init() {
	commands = append(commands,
		command{
			name:    "record",
			args:    "[-o FILE] [-d DURATION] SENSOR[@HZ]...",
			summary: "record sensors to a file, for replay",
			help: `Subscribes to each SENSOR (at HZ, or every event) and writes every event to
FILE, until -d elapses or you press Ctrl-C. Replay it with "sensord replay".

` + sensorHelp + `

The file is JSON lines: a header naming the sensors, then one line per
event with its sensor, timestamp and values.

Examples:
  sensord record -d 30s accelerometer gyroscope
  sensord record -o walk.jsonl gravity@60 rotation_vector@60 step_detector`,
			flags: func(fs *flag.FlagSet) {
				recOut = fs.String("o", "", "file to write (default: recording-DATE-TIME.jsonl)")
				recDur = fs.Duration("d", 0, "stop after this long (0 = until Ctrl-C)")
			},
			run: func(c *client.Client, fs *flag.FlagSet) error { return record(c, fs.Args()) },
		},
		command{
			name:     "replay",
			args:     "[-addr ADDR] [-loop] [-speed X] FILE",
			summary:  "serve a recording as if it were the phone's sensors",
			noClient: true,
			help: `Serves FILE (from "sensord record") on ADDR with the same protocol as the
app, so any program can run against it: point it there with SENSORD_ADDR.
Events keep their recorded timing; each sensor starts playing when a program
subscribes to it, in step with the others.

Examples:
  sensord replay -loop walk.jsonl &
  SENSORD_ADDR=127.0.0.1:47475 sensordemo compass
  SENSORD_ADDR=127.0.0.1:47475 sensord stream gravity`,
			flags: func(fs *flag.FlagSet) {
				playAddr = fs.String("addr", "127.0.0.1:47475", "address to serve on")
				playLoop = fs.Bool("loop", false, "start over at the end")
				playRate = fs.Float64("speed", 1, "playback speed")
			},
			run: func(_ *client.Client, fs *flag.FlagSet) error {
				if fs.NArg() != 1 {
					return errUsage
				}
				return replay(fs.Arg(0), *playAddr, *playLoop, *playRate)
			},
		},
	)
}

func record(c *client.Client, args []string) error {
	if len(args) == 0 {
		return errUsage
	}
	all, err := c.Sensors()
	if err != nil {
		return err
	}
	byName := map[string]proto.Sensor{}
	for _, s := range all {
		byName[s.Name] = s
	}
	var hdr recHeader
	hdr.Version = 1
	var subs []*client.Subscription
	for _, a := range args {
		name, hz := a, 0.0
		if i := strings.LastIndexByte(a, '@'); i > 0 {
			if hz, err = strconv.ParseFloat(a[i+1:], 64); err != nil {
				return fmt.Errorf("%q: bad rate", a)
			}
			name = a[:i]
		}
		sub, err := c.Subscribe(name, hz)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		defer sub.Close()
		subs = append(subs, sub)
		hdr.Sensors = append(hdr.Sensors, byName[sub.Sensor])
	}

	path := *recOut
	if path == "" {
		path = "recording-" + time.Now().Format("20060102-150405") + ".jsonl"
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	if err := enc.Encode(hdr); err != nil {
		return err
	}

	events := make(chan recEvent, 1024)
	var wg sync.WaitGroup
	for i, sub := range subs {
		wg.Add(1)
		go func(i int, sub *client.Subscription) {
			defer wg.Done()
			for ev := range sub.C {
				events <- recEvent{S: i, T: ev.T, V: ev.V}
			}
		}(i, sub)
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	var timeout <-chan time.Time
	if *recDur > 0 {
		timeout = time.After(*recDur)
	}
	fmt.Fprintf(os.Stderr, "recording %d sensors to %s (Ctrl-C to stop)\n", len(subs), path)
	n := 0
	start := time.Now()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
loop:
	for {
		select {
		case ev := <-events:
			for i, x := range ev.V { // JSON has no NaN
				if math.IsNaN(x) || math.IsInf(x, 0) {
					ev.V[i] = 0
				}
			}
			if err := enc.Encode(ev); err != nil {
				return err
			}
			n++
		case <-tick.C:
			fmt.Fprintf(os.Stderr, "\r%d events, %s", n, time.Since(start).Round(time.Second))
		case <-stop:
			break loop
		case <-timeout:
			break loop
		}
	}
	fmt.Fprintf(os.Stderr, "\r%d events in %s -> %s\n", n, time.Since(start).Round(time.Second), path)
	if err := w.Flush(); err != nil {
		return err
	}
	return f.Close()
}

// loadRecording reads a recording: the sensors, and each one's events.
func loadRecording(path string) (recHeader, [][]recEvent, error) {
	var hdr recHeader
	f, err := os.Open(path)
	if err != nil {
		return hdr, nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(bufio.NewReader(f))
	if err := dec.Decode(&hdr); err != nil || hdr.Version != 1 {
		return hdr, nil, fmt.Errorf("%s: not a sensord recording", path)
	}
	evs := make([][]recEvent, len(hdr.Sensors))
	for {
		var ev recEvent
		if err := dec.Decode(&ev); err != nil {
			if !errors.Is(err, io.EOF) {
				return hdr, nil, fmt.Errorf("%s: %w", path, err)
			}
			break
		}
		if ev.S >= 0 && ev.S < len(evs) {
			evs[ev.S] = append(evs[ev.S], ev)
		}
	}
	return hdr, evs, nil
}

// player is a hub backend that plays a recording. All sensors follow one
// clock, so they stay in step whenever each is subscribed.
type player struct {
	infos    []hub.Info
	evs      [][]recEvent
	t0, span int64 // the recording's first timestamp and its length, ns
	start    time.Time
	speed    float64
	loop     bool
	dispatch func(handle int32, t int64, v []float64)

	mu      sync.Mutex
	playing map[int32]chan struct{}
}

func newPlayer(hdr recHeader, evs [][]recEvent, speed float64, loop bool) *player {
	p := &player{evs: evs, speed: speed, loop: loop, start: time.Now(), playing: map[int32]chan struct{}{}}
	first, last := int64(math.MaxInt64), int64(math.MinInt64)
	for _, e := range evs {
		if len(e) > 0 {
			first, last = min(first, e[0].T), max(last, e[len(e)-1].T)
		}
	}
	if first > last {
		first, last = 0, 0
	}
	// A lap lasts one typical gap past the last event, so the first event
	// of the next lap follows as the others do.
	total := 0
	for _, e := range evs {
		total += len(e)
	}
	gap := (last - first) / int64(max(total-1, 1))
	p.t0, p.span = first, max(last-first+gap, 1)
	for i, s := range hdr.Sensors {
		info := hub.Info{
			Handle: int32(i + 1), Name: s.Name, Vendor: s.Vendor, Type: s.Type, TypeID: int32(s.TypeID),
			Wakeup: s.Wakeup, Default: s.Default,
			Precise: true, // the values were already rounded when recorded
		}
		switch s.Mode {
		case "on-change":
			info.Mode = hub.OnChange
		case "one-shot":
			info.Mode = hub.OneShot
		case "special":
			info.Mode = hub.Special
		default:
			info.Mode = hub.Continuous
		}
		if s.MaxHz > 0 {
			info.MinDelayUs = int32(1e6 / s.MaxHz)
		}
		p.infos = append(p.infos, info)
	}
	return p
}

func (p *player) Sensors() []hub.Info { return p.infos }

// now is the playback position in recording time, ns, counting loops.
func (p *player) now() int64 {
	return int64(float64(time.Since(p.start)) * p.speed)
}

func (p *player) Enable(h int32, periodUs int32) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.playing[h]; ok {
		return nil
	}
	stop := make(chan struct{})
	p.playing[h] = stop
	go p.play(h, stop)
	return nil
}

func (p *player) SetPeriod(h int32, periodUs int32) error { return nil } // the hub downsamples

func (p *player) Disable(h int32) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if stop, ok := p.playing[h]; ok {
		close(stop)
		delete(p.playing, h)
	}
	return nil
}

// play sends sensor h's events as the playback clock reaches them. Their
// timestamps become the playback time, so they read as live and keep
// increasing across loops.
func (p *player) play(h int32, stop chan struct{}) {
	evs := p.evs[h-1]
	if len(evs) == 0 {
		return
	}
	pos := p.now()
	lap := pos / p.span
	if !p.loop && lap > 0 {
		return // the recording has ended
	}
	// The first event at or after the current position.
	i := 0
	for i < len(evs) && evs[i].T-p.t0 < pos%p.span {
		i++
	}
	for {
		if i == len(evs) {
			if !p.loop {
				return
			}
			i, lap = 0, lap+1
		}
		due := lap*p.span + evs[i].T - p.t0
		if wait := time.Duration(float64(due-p.now()) / p.speed); wait > 0 {
			select {
			case <-stop:
				return
			case <-time.After(wait):
			}
		} else {
			select {
			case <-stop:
				return
			default:
			}
		}
		p.dispatch(h, due, append([]float64(nil), evs[i].V...))
		i++
	}
}

func replay(path, addr string, loop bool, speed float64) error {
	if speed <= 0 {
		return fmt.Errorf("speed must be positive")
	}
	hdr, evs, err := loadRecording(path)
	if err != nil {
		return err
	}
	p := newPlayer(hdr, evs, speed, loop)
	h := hub.New(p)
	p.dispatch = h.Dispatch
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	total := 0
	for _, e := range evs {
		total += len(e)
	}
	fmt.Fprintf(os.Stderr, "replaying %d sensors, %d events over %s, on %s\n",
		len(hdr.Sensors), total, time.Duration(p.span).Round(time.Millisecond), addr)
	fmt.Fprintf(os.Stderr, "run programs with SENSORD_ADDR=%s\n", addr)
	return (&server.Server{Hub: h}).Serve(ln)
}

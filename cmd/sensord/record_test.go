package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TomoBossi/sensord/client"
	"github.com/TomoBossi/sensord/internal/hub"
	"github.com/TomoBossi/sensord/internal/server"
	"github.com/TomoBossi/sensord/proto"
)

// A recording replays through the real hub and server: values in order,
// spaced as recorded, looping, and sensors in step with each other.
func TestReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.jsonl")
	f, _ := os.Create(path)
	enc := json.NewEncoder(f)
	enc.Encode(recHeader{Version: 1, Sensors: []proto.Sensor{
		{Name: "ACC", Type: "accelerometer", MaxHz: 100, Mode: "continuous", Default: true},
		{Name: "STEP", Type: "step_detector", Mode: "special", Default: true},
	}})
	const t0 = int64(5e12)
	for i := 0; i < 20; i++ { // 20 readings, 10 ms apart
		enc.Encode(recEvent{S: 0, T: t0 + int64(i)*1e7, V: []float64{float64(i), 0, 9.8}})
	}
	enc.Encode(recEvent{S: 1, T: t0 + 1e8, V: []float64{1}}) // a step at 100 ms
	f.Close()

	hdr, evs, err := loadRecording(path)
	if err != nil || len(evs[0]) != 20 || len(evs[1]) != 1 {
		t.Fatalf("load: %v %d", err, len(evs))
	}
	p := newPlayer(hdr, evs, 1, true)
	h := hub.New(p)
	p.dispatch = h.Dispatch
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go (&server.Server{Hub: h}).Serve(ln)

	c, err := client.Dial(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sub, err := c.Subscribe("accelerometer", 0)
	if err != nil || sub.Sensor != "ACC" {
		t.Fatalf("sub: %v %q", err, sub.Sensor)
	}
	var got []Event
	deadline := time.After(2 * time.Second)
	for len(got) < 45 {
		select {
		case ev := <-sub.C:
			got = append(got, Event{ev.T, ev.V[0]})
		case <-deadline:
			t.Fatalf("only %d events", len(got))
		}
	}
	for i := 1; i < len(got); i++ {
		if d := got[i].t - got[i-1].t; d <= 0 {
			t.Fatalf("timestamps not increasing at %d: %v", i, got[i-1:i+1])
		}
		want := got[i-1].x + 1
		if want == 20 {
			want = 0 // looped
		}
		if got[i].x != want {
			t.Fatalf("event %d is %v after %v", i, got[i].x, got[i-1].x)
		}
	}
	if d := time.Duration(got[5].t - got[4].t); d < 9*time.Millisecond || d > 11*time.Millisecond {
		t.Errorf("readings %v apart, recorded 10ms", d)
	}
	// A late subscriber joins at the current position.
	step, err := c.Subscribe("step_detector", 0)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-step.C:
		if pos := (ev.T - 0) % p.span; pos < 9e7 || pos > 1.1e8 {
			t.Errorf("step at %v into the loop, recorded at 100ms", time.Duration(pos))
		}
	case <-time.After(time.Second):
		t.Error("no step")
	}
}

type Event struct {
	t int64
	x float64
}

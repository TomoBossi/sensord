package server

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/TomoBossi/sensord/client"
	"github.com/TomoBossi/sensord/internal/hub"
)

// tickBackend emits a fake 100 Hz accelerometer while enabled.
type tickBackend struct {
	mu      sync.Mutex
	h       *hub.Hub
	enabled map[int32]chan struct{}
}

func (b *tickBackend) Sensors() []hub.Info {
	return []hub.Info{{Handle: 1, Name: "acc", Type: "accelerometer", TypeID: 1, MinDelayUs: 2500, Default: true}}
}

func (b *tickBackend) Enable(h, p int32) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	stop := make(chan struct{})
	b.enabled[h] = stop
	go func() {
		tk := time.NewTicker(10 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-tk.C:
				b.h.Dispatch(h, now.UnixNano(), []float64{0, 0, 9.8})
			}
		}
	}()
	return nil
}

func (b *tickBackend) SetPeriod(h, p int32) error { return nil }

func (b *tickBackend) Disable(h int32) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	close(b.enabled[h])
	delete(b.enabled, h)
	return nil
}

func (b *tickBackend) on() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.enabled) > 0
}

func start(t *testing.T) (*tickBackend, string) {
	be := &tickBackend{enabled: map[int32]chan struct{}{}}
	be.h = hub.New(be)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go (&Server{Hub: be.h}).Serve(ln)
	return be, ln.Addr().String()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for " + what)
}

func TestEndToEnd(t *testing.T) {
	be, addr := start(t)
	c, err := client.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.Ping(); err != nil {
		t.Fatal(err)
	}
	sensors, err := c.Sensors()
	if err != nil || len(sensors) != 1 || sensors[0].MaxHz != 400 {
		t.Fatalf("sensors %+v, %v", sensors, err)
	}
	if _, err := c.Subscribe("nope", 1); err == nil {
		t.Fatal("unknown sensor: want error")
	}

	sub, err := c.Subscribe("accelerometer", 20)
	if err != nil {
		t.Fatal(err)
	}
	if sub.Sensor != "acc" || sub.Hz != 20 {
		t.Fatalf("granted %s at %v Hz", sub.Sensor, sub.Hz)
	}
	n := 0
	deadline := time.After(time.Second)
loop:
	for {
		select {
		case ev := <-sub.C:
			if len(ev.V) != 3 || ev.V[2] != 9.8 {
				t.Fatalf("event %+v", ev)
			}
			n++
		case <-deadline:
			break loop
		}
	}
	if n < 17 || n > 23 {
		t.Errorf("got %d events in 1 s at 20 Hz", n)
	}

	if err := sub.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "sensor off after unsub", func() bool { return !be.on() })
	for range sub.C { // must be closed
	}
}

func TestDisconnectStopsSensor(t *testing.T) {
	be, addr := start(t)
	c, _ := client.Dial(addr)
	if _, err := c.Subscribe("accelerometer", 0); err != nil {
		t.Fatal(err)
	}
	if !be.on() {
		t.Fatal("sensor not enabled")
	}
	c.Close() // no unsub: simulates a client that crashed
	waitFor(t, "sensor off after disconnect", func() bool { return !be.on() })
}

func TestCloseWithoutReading(t *testing.T) {
	_, addr := start(t)
	c, _ := client.Dial(addr)
	defer c.Close()
	sub, _ := c.Subscribe("accelerometer", 0)
	time.Sleep(3 * time.Second) // 100 Hz fills the 256-event buffer
	done := make(chan error)
	go func() { done <- sub.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close deadlocked with a full event buffer")
	}
}

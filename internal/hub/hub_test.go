package hub

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeBackend struct {
	period map[int32]int32 // enabled sensors -> registered period
	calls  []string
}

func (f *fakeBackend) Sensors() []Info {
	return []Info{
		{Handle: 1, Name: "bmi3xy_acc", Type: "accelerometer", TypeID: 1, MinDelayUs: 2500, Default: true},
		{Handle: 2, Name: "ltr569_l", Type: "light", TypeID: 5, Mode: OnChange, Default: true},
		{Handle: 3, Name: "SIGNIFICANT_MOTION", Type: "significant_motion", TypeID: 17, Mode: OneShot, Default: true},
		{Handle: 4, Name: "ACC_WAKEUP", Type: "accelerometer", TypeID: 1, MinDelayUs: 5000, Wakeup: true},
	}
}

func (f *fakeBackend) Enable(h, p int32) error {
	f.period[h] = p
	f.calls = append(f.calls, fmt.Sprintf("enable %d %d", h, p))
	return nil
}

func (f *fakeBackend) SetPeriod(h, p int32) error {
	f.period[h] = p
	f.calls = append(f.calls, fmt.Sprintf("period %d %d", h, p))
	return nil
}

func (f *fakeBackend) Disable(h int32) error {
	delete(f.period, h)
	f.calls = append(f.calls, fmt.Sprintf("disable %d", h))
	return nil
}

type fakeClient struct{ lines []string }

func (c *fakeClient) Send(line []byte) bool {
	c.lines = append(c.lines, string(line))
	return true
}

func newHub() (*Hub, *fakeBackend) {
	be := &fakeBackend{period: map[int32]int32{}}
	return New(be), be
}

// feed dispatches secs seconds of events at srcHz with timestamp jitter.
func feed(h *Hub, handle int32, srcHz float64, secs float64, t0 int64) int64 {
	r := rand.New(rand.NewSource(1))
	period := 1e9 / srcHz
	n := int(srcHz * secs)
	t := t0
	for i := 0; i < n; i++ {
		t = t0 + int64(float64(i)*period) + int64((r.Float64()-0.5)*0.1*period)
		h.Dispatch(handle, t, []float64{0.1, 0.2, 9.8})
	}
	return t
}

func TestResolve(t *testing.T) {
	h, _ := newHub()
	for spec, want := range map[string]string{
		"accelerometer": "bmi3xy_acc",
		"ACCELEROMETER": "bmi3xy_acc",
		"acc_wakeup":    "ACC_WAKEUP",
		"bmi3xy_acc":    "bmi3xy_acc",
	} {
		info, _, err := h.Subscribe(&fakeClient{}, 1, spec, 10)
		if err != nil || info.Name != want {
			t.Errorf("%q -> %q, %v; want %q", spec, info.Name, err, want)
		}
	}
	if _, _, err := h.Subscribe(&fakeClient{}, 1, "nope", 10); err != ErrUnknownSensor {
		t.Errorf("unknown sensor: got %v", err)
	}
}

func TestRegistrationFollowsFastestSubscriber(t *testing.T) {
	h, be := newHub()
	a, b := &fakeClient{}, &fakeClient{}

	if _, hz, _ := h.Subscribe(a, 1, "accelerometer", 10); hz != 10 {
		t.Errorf("granted %v, want 10", hz)
	}
	if be.period[1] != 100000 {
		t.Fatalf("period %d, want 100000", be.period[1])
	}
	h.Subscribe(b, 1, "accelerometer", 100)
	if be.period[1] != 10000 {
		t.Fatalf("period %d, want 10000", be.period[1])
	}
	h.Unsubscribe(b, 1)
	if be.period[1] != 100000 {
		t.Fatalf("after unsub: period %d, want 100000", be.period[1])
	}
	h.Drop(a)
	if _, on := be.period[1]; on {
		t.Fatal("sensor still enabled with no subscribers")
	}
	want := "enable 1 100000,period 1 10000,period 1 100000,disable 1"
	if got := strings.Join(be.calls, ","); got != want {
		t.Errorf("calls %s\nwant  %s", got, want)
	}
}

func TestGrantedRateIsExact(t *testing.T) {
	h, _ := newHub()
	if _, hz, _ := h.Subscribe(&fakeClient{}, 1, "accelerometer", 60); hz != 60 {
		t.Errorf("granted %v, want exactly 60", hz)
	}
}

func TestClampToSensorMax(t *testing.T) {
	h, be := newHub()
	_, hz, _ := h.Subscribe(&fakeClient{}, 1, "accelerometer", 1000)
	if hz != 400 || be.period[1] != 2500 {
		t.Errorf("granted %v at period %d, want 400 at 2500", hz, be.period[1])
	}
	_, hz, _ = h.Subscribe(&fakeClient{}, 2, "accelerometer", 0)
	if hz != 400 {
		t.Errorf("hz 0: granted %v, want 400", hz)
	}
}

func TestDownsampling(t *testing.T) {
	cases := []struct {
		srcHz, wantHz float64
	}{
		{100, 10},
		{100, 50},
		{100.5, 50}, // hardware slightly fast: must not collapse to 33 Hz
		{99.2, 50},  // slightly slow
		{100, 30},   // not a divisor
		{100, 100},
	}
	for _, c := range cases {
		h, _ := newHub()
		fast, slow := &fakeClient{}, &fakeClient{}
		h.Subscribe(fast, 1, "accelerometer", c.srcHz)
		h.Subscribe(slow, 7, "accelerometer", c.wantHz)
		feed(h, 1, c.srcHz, 10, 1e12)
		got := float64(len(slow.lines)) / 10
		if got < c.wantHz*0.97 || got > c.wantHz*1.03 {
			t.Errorf("src %v Hz, sub %v Hz: delivered %.1f Hz", c.srcHz, c.wantHz, got)
		}
		if len(fast.lines) != int(c.srcHz*10) {
			t.Errorf("src %v Hz: full-rate subscriber got %d of %d", c.srcHz, len(fast.lines), int(c.srcHz*10))
		}
	}
}

// The hardware may ignore a slow requested period: the accelerometer on this
// phone never goes below 12.5 Hz. A 5 Hz subscriber must still get 5 Hz.
func TestSourceFasterThanRegistered(t *testing.T) {
	h, be := newHub()
	c := &fakeClient{}
	h.Subscribe(c, 1, "accelerometer", 5)
	if be.period[1] != 200000 {
		t.Fatalf("period %d, want 200000", be.period[1])
	}
	feed(h, 1, 12.5, 20, 1e12)
	if got := float64(len(c.lines)) / 20; got < 4.8 || got > 5.2 {
		t.Errorf("delivered %.2f Hz, want 5", got)
	}
}

func TestResyncAfterGap(t *testing.T) {
	h, _ := newHub()
	c := &fakeClient{}
	h.Subscribe(c, 1, "accelerometer", 10)
	end := feed(h, 1, 100, 2, 1e12)
	// A 5 s gap (e.g. the sensor paused) must not cause a burst afterwards.
	before := len(c.lines)
	feed(h, 1, 100, 2, end+5e9)
	if got := len(c.lines) - before; got < 19 || got > 21 {
		t.Errorf("after gap: %d events in 2 s, want ~20", got)
	}
}

func TestEventLine(t *testing.T) {
	h, _ := newHub()
	c := &fakeClient{}
	h.Subscribe(c, 42, "light", 0)
	h.Dispatch(2, 123456789, []float64{3.5})
	h.Dispatch(1, 1, []float64{1}) // other sensor, nobody subscribed
	if len(c.lines) != 1 || c.lines[0] != `{"id":42,"t":123456789,"v":[3.5]}`+"\n" {
		t.Errorf("lines %q", c.lines)
	}
}

func TestDuplicateID(t *testing.T) {
	h, _ := newHub()
	c := &fakeClient{}
	h.Subscribe(c, 1, "accelerometer", 10)
	if _, _, err := h.Subscribe(c, 1, "light", 0); err == nil {
		t.Error("reused id: want error")
	}
}

// lockedBackend is a fakeBackend safe to inspect while hub timers run.
type lockedBackend struct {
	mu sync.Mutex
	fakeBackend
}

func (b *lockedBackend) Enable(h, p int32) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fakeBackend.Enable(h, p)
}

func (b *lockedBackend) SetPeriod(h, p int32) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fakeBackend.SetPeriod(h, p)
}

func (b *lockedBackend) Disable(h int32) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fakeBackend.Disable(h)
}

func (b *lockedBackend) on(h int32) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.period[h]
	return ok
}

func newTimedHub() (*Hub, *lockedBackend) {
	be := &lockedBackend{fakeBackend: fakeBackend{period: map[int32]int32{}}}
	h := New(be)
	h.Linger = 50 * time.Millisecond
	h.GetTimeout = 200 * time.Millisecond
	h.RearmDelay = 10 * time.Millisecond
	return h, be
}

func TestGetPowersOnWaitsAndLingers(t *testing.T) {
	h, be := newTimedHub()
	go func() {
		for !be.on(1) {
			time.Sleep(time.Millisecond)
		}
		h.Dispatch(1, 1000, []float64{1, 2, 3})
	}()
	info, ts, v, err := h.Get("accelerometer", 0)
	if err != nil || info.Name != "bmi3xy_acc" || ts != 1000 || len(v) != 3 || v[2] != 3 {
		t.Fatalf("got %s %d %v %v", info.Name, ts, v, err)
	}
	if be.period[1] != 20000 {
		t.Errorf("polled at period %d, want 20000 (50 Hz)", be.period[1])
	}

	// A second Get while warm answers from the cache without waiting.
	h.Dispatch(1, 2000, []float64{4, 5, 6})
	if _, ts, _, _ := h.Get("accelerometer", 0); ts != 2000 {
		t.Errorf("warm get: t=%d, want 2000", ts)
	}

	// Gets keep it on; once they stop, it powers down after Linger.
	time.Sleep(30 * time.Millisecond)
	h.Get("accelerometer", 0)
	time.Sleep(30 * time.Millisecond)
	if !be.on(1) {
		t.Fatal("powered down while gets kept coming")
	}
	time.Sleep(60 * time.Millisecond)
	if be.on(1) {
		t.Fatal("still powered after linger")
	}
}

func TestGetSharesWithSubscribers(t *testing.T) {
	h, be := newTimedHub()
	c := &fakeClient{}
	h.Subscribe(c, 1, "accelerometer", 100)
	h.Dispatch(1, 1000, []float64{1, 2, 3})
	if _, ts, _, err := h.Get("accelerometer", 0); err != nil || ts != 1000 {
		t.Fatalf("get: %d %v", ts, err)
	}
	if be.period[1] != 10000 {
		t.Errorf("period %d: a 50 Hz poll must not slow a 100 Hz subscriber", be.period[1])
	}
	if len(c.lines) != 1 {
		t.Errorf("subscriber got %d lines, want 1 (the poll gets none)", len(c.lines))
	}
	time.Sleep(80 * time.Millisecond)
	if !be.on(1) {
		t.Error("poll ending turned off a sensor that still has a subscriber")
	}
}

func TestGetTimeout(t *testing.T) {
	h, _ := newTimedHub()
	if _, _, _, err := h.Get("light", 0); err == nil {
		t.Fatal("no reading: want timeout error")
	}
	if _, _, _, err := h.Get("significant_motion", 0); err == nil {
		t.Fatal("one-shot: want error")
	}
}

func TestOneShotRearms(t *testing.T) {
	h, be := newTimedHub()
	c := &fakeClient{}
	if _, _, err := h.Subscribe(c, 1, "significant_motion", 5); err != nil {
		t.Fatal(err)
	}
	if be.period[3] != 0 {
		t.Errorf("one-shot registered at %d, want 0", be.period[3])
	}
	for i := 0; i < 3; i++ {
		h.Dispatch(3, int64(i+1)*1e9, []float64{1})
		time.Sleep(30 * time.Millisecond)
		if !be.on(3) {
			t.Fatalf("trigger %d: not re-armed", i+1)
		}
	}
	if len(c.lines) != 3 {
		t.Errorf("got %d triggers, want 3", len(c.lines))
	}
	h.Drop(c)
	h.Dispatch(3, 9e9, []float64{1}) // a late trigger after the last reader left
	time.Sleep(30 * time.Millisecond)
	if be.on(3) {
		t.Error("re-armed with no subscribers")
	}
}

func TestOnAwakeOnlyForNonWakeupSensors(t *testing.T) {
	h, _ := newHub()
	var calls []bool
	h.OnAwake = func(on bool) { calls = append(calls, on) }
	a, b := &fakeClient{}, &fakeClient{}

	h.Subscribe(a, 1, "accelerometer", 10)
	h.Subscribe(b, 1, "accelerometer", 100) // same sensor: no new call
	h.Subscribe(a, 2, "light", 0)           // second non-wakeup sensor: no new call
	h.Subscribe(a, 3, "ACC_WAKEUP", 0)      // wakeup sensor: never counts
	h.Subscribe(a, 4, "significant_motion", 0)
	if len(calls) != 1 || !calls[0] {
		t.Fatalf("after subscribing: calls %v, want [true]", calls)
	}
	h.Drop(b)
	h.Unsubscribe(a, 2)
	if len(calls) != 1 {
		t.Fatalf("released while the accelerometer is still on: %v", calls)
	}
	h.Drop(a)
	if len(calls) != 2 || calls[1] {
		t.Fatalf("after dropping everyone: calls %v, want [true false]", calls)
	}

	// Wakeup-only use never takes the wake lock.
	calls = nil
	h.Subscribe(a, 5, "ACC_WAKEUP", 0)
	h.Drop(a)
	if len(calls) != 0 {
		t.Errorf("wakeup sensor triggered OnAwake: %v", calls)
	}
}

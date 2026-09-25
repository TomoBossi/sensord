package hub

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
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
	if _, _, err := h.Subscribe(&fakeClient{}, 1, "significant_motion", 0); err == nil {
		t.Error("one-shot: want error")
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

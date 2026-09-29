// Package hub multiplexes sensor subscriptions onto a single registration per
// sensor.
//
// Each sensor is enabled only while at least one subscription needs it, at the
// fastest period any subscription asks for. Every subscription then receives
// its own rate by downsampling on event timestamps. The hub knows nothing about
// Android or sockets; those sit behind Backend and Client.
package hub

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Mode is a sensor's reporting mode.
type Mode int

const (
	Continuous Mode = iota
	OnChange
	OneShot
	Special
)

func (m Mode) String() string {
	switch m {
	case Continuous:
		return "continuous"
	case OnChange:
		return "on-change"
	case OneShot:
		return "one-shot"
	default:
		return "special"
	}
}

// Info describes a sensor as reported by the backend.
type Info struct {
	Handle     int32
	Name       string
	Vendor     string
	Type       string // "accelerometer", or the full string for vendor types
	TypeID     int32
	MinDelayUs int32 // fastest period; 0 for on-change and one-shot sensors
	Mode       Mode
	Wakeup     bool
	Default    bool
	Precise    bool // values are float64 (location); hardware sensors are float32
}

// MaxHz is the fastest rate the sensor supports, or 0 if it has none.
func (i Info) MaxHz() float64 {
	if i.MinDelayUs <= 0 {
		return 0
	}
	return 1e6 / float64(i.MinDelayUs)
}

// Backend drives the actual sensors. Calls are serialized by the hub.
type Backend interface {
	Sensors() []Info
	Enable(handle int32, periodUs int32) error
	SetPeriod(handle int32, periodUs int32) error
	Disable(handle int32) error
}

// Client receives event lines. Send must not block; it reports false when the
// line was dropped.
type Client interface {
	Send(line []byte) bool
}

var ErrUnknownSensor = errors.New("unknown sensor")

type sensor struct {
	info     Info
	subs     map[*sub]struct{}
	periodUs int32 // registered period; -1 while disabled
	lastT    int64 // timestamp of the previous event
	avgDt    int64 // smoothed interval between events, ns; 0 until measured

	// Latest reading, for Get. Valid only while the sensor is enabled.
	hasLast bool
	lastV   []float64
	waiters []chan struct{} // closed on the next event

	// Get keeps the sensor powered through a pseudo-subscription (c == nil)
	// until pollUntil.
	poll      *sub
	pollUntil time.Time
	pollTimer *time.Timer
}

type sub struct {
	c        Client
	id       int64
	s        *sensor
	regUs    int32 // registration period this subscription asks for
	periodNs int64 // delivery period; 0 = every event
	due      int64 // next delivery time; 0 until the first event
	prefix   []byte
}

// Hub is safe for concurrent use.
type Hub struct {
	mu       sync.Mutex
	be       Backend
	sensors  []*sensor
	byHandle map[int32]*sensor
	byName   map[string]*sensor // lowercased exact names
	byType   map[string]*sensor // default sensor per type string
	clients  map[Client]map[int64]*sub
	dropped  uint64

	// Linger is how long a sensor stays powered after the last Get, so a
	// polling loop keeps it warm. GetTimeout bounds the wait for a first
	// reading. Set both before use.
	Linger     time.Duration
	GetTimeout time.Duration

	// OnAwake, if set, is called with true when the first non-wakeup sensor
	// is enabled and with false when the last one is disabled, with the hub
	// locked. Non-wakeup sensors lose events while the CPU sleeps, so the app
	// holds a partial wake lock in between; wakeup sensors wake the CPU
	// themselves and don't count.
	OnAwake func(bool)
	awake   int // enabled non-wakeup sensors

	// RearmDelay is the pause before re-enabling a one-shot sensor after it
	// fires. Android disables it right after delivering the event; enabling
	// it again too early can race with that.
	RearmDelay time.Duration
}

func New(be Backend) *Hub {
	h := &Hub{
		be:       be,
		byHandle: map[int32]*sensor{},
		byName:   map[string]*sensor{},
		byType:   map[string]*sensor{},
		clients:  map[Client]map[int64]*sub{},

		Linger:     2 * time.Second,
		GetTimeout: 5 * time.Second,
		RearmDelay: 100 * time.Millisecond,
	}
	for _, info := range be.Sensors() {
		s := &sensor{info: info, subs: map[*sub]struct{}{}, periodUs: -1}
		h.sensors = append(h.sensors, s)
		h.byHandle[info.Handle] = s
		h.byName[strings.ToLower(info.Name)] = s
		if info.Default {
			h.byType[strings.ToLower(info.Type)] = s
		}
	}
	// A type with no default sensor (a vendor's second light sensor, say)
	// still resolves, to its first sensor.
	for _, s := range h.sensors {
		k := strings.ToLower(s.info.Type)
		if _, ok := h.byType[k]; !ok {
			h.byType[k] = s
		}
	}
	return h
}

// Sensors lists every sensor the backend reported.
func (h *Hub) Sensors() []Info {
	out := make([]Info, len(h.sensors))
	for i, s := range h.sensors {
		out[i] = s.info
	}
	return out
}

func (h *Hub) resolve(spec string) *sensor {
	k := strings.ToLower(spec)
	if s, ok := h.byName[k]; ok {
		return s
	}
	return h.byType[k]
}

// Subscribe starts delivering events of the sensor named by spec (exact name
// or type) to c, tagged with id, at hz (0 = every event). It returns the
// sensor and the delivery rate actually granted (0 = every event).
func (h *Hub) Subscribe(c Client, id int64, spec string, hz float64) (Info, float64, error) {
	if hz < 0 || math.IsNaN(hz) || math.IsInf(hz, 0) {
		return Info{}, 0, fmt.Errorf("invalid hz %v", hz)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.resolve(spec)
	if s == nil {
		return Info{}, 0, ErrUnknownSensor
	}
	if h.clients[c][id] != nil {
		return Info{}, 0, fmt.Errorf("id %d already in use", id)
	}

	b := &sub{c: c, id: id, s: s}
	b.prefix = strconv.AppendInt([]byte(`{"id":`), id, 10)
	b.prefix = append(b.prefix, ',')
	granted := 0.0
	min := max(s.info.MinDelayUs, 0) // -1 for one-shot sensors
	switch {
	case hz == 0 || s.info.Mode == OneShot:
		b.regUs = min
		granted = s.info.MaxHz()
	default:
		periodUs := 1e6 / hz
		granted = hz
		if periodUs < float64(min) {
			periodUs = float64(min)
			granted = s.info.MaxHz()
		}
		if periodUs > math.MaxInt32 {
			periodUs = math.MaxInt32
		}
		b.regUs = int32(periodUs)
		b.periodNs = int64(periodUs * 1e3)
	}

	s.subs[b] = struct{}{}
	if err := h.reconcile(s); err != nil {
		delete(s.subs, b)
		return Info{}, 0, err
	}
	if h.clients[c] == nil {
		h.clients[c] = map[int64]*sub{}
	}
	h.clients[c][id] = b
	return s.info, granted, nil
}

// Unsubscribe stops the subscription id of c.
func (h *Hub) Unsubscribe(c Client, id int64) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.clients[c][id]
	if b == nil {
		return fmt.Errorf("no subscription %d", id)
	}
	delete(h.clients[c], id)
	if len(h.clients[c]) == 0 {
		delete(h.clients, c)
	}
	delete(b.s.subs, b)
	return h.reconcile(b.s)
}

// Drop removes every subscription of c. After it returns, c gets no more Sends.
func (h *Hub) Drop(c Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	touched := map[*sensor]struct{}{}
	for _, b := range h.clients[c] {
		delete(b.s.subs, b)
		touched[b.s] = struct{}{}
	}
	delete(h.clients, c)
	for s := range touched {
		h.reconcile(s) // errors here have nowhere to go; the sensor state stays consistent
	}
}

// reconcile brings the sensor's registration in line with its subscriptions.
func (h *Hub) reconcile(s *sensor) error {
	if len(s.subs) == 0 {
		if s.periodUs < 0 {
			return nil
		}
		s.periodUs = -1
		s.lastT, s.avgDt = 0, 0
		s.hasLast = false
		h.setAwake(s, false)
		return h.be.Disable(s.info.Handle)
	}
	want := int32(math.MaxInt32)
	for b := range s.subs {
		if b.regUs < want {
			want = b.regUs
		}
	}
	switch {
	case s.periodUs < 0:
		if err := h.be.Enable(s.info.Handle, want); err != nil {
			return err
		}
		h.setAwake(s, true)
	case want != s.periodUs:
		if err := h.be.SetPeriod(s.info.Handle, want); err != nil {
			return err
		}
	default:
		return nil
	}
	s.periodUs = want
	return nil
}

// setAwake counts s being enabled (on) or disabled for OnAwake. Called with
// h.mu held, on every enabled <-> disabled transition of s.
func (h *Hub) setAwake(s *sensor, on bool) {
	if s.info.Wakeup {
		return
	}
	if on {
		h.awake++
		if h.awake == 1 && h.OnAwake != nil {
			h.OnAwake(true)
		}
		return
	}
	h.awake--
	if h.awake == 0 && h.OnAwake != nil {
		h.OnAwake(false)
	}
}

// Dispatch delivers one event of the sensor with the given handle to every
// subscription that is due.
func (h *Hub) Dispatch(handle int32, t int64, v []float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.byHandle[handle]
	if s == nil || len(s.subs) == 0 {
		return
	}

	// Sensor clocks jitter, and the hardware rarely runs at exactly the
	// registered period: it may be slightly off, or clamped (the accelerometer
	// here won't go below 12.5 Hz). Accepting events up to half the measured
	// source interval early keeps e.g. 50 Hz from a slightly fast 100 Hz
	// source at 50, not 33.
	if s.lastT != 0 && t > s.lastT {
		dt := t - s.lastT
		if s.avgDt == 0 {
			s.avgDt = dt
		} else {
			s.avgDt += (dt - s.avgDt) / 8
		}
	}
	s.lastT = t
	tol := s.avgDt / 2

	s.lastV = append(s.lastV[:0], v...)
	s.hasLast = true
	for _, w := range s.waiters {
		close(w)
	}
	s.waiters = nil
	if s.info.Mode == OneShot {
		defer h.rearm(s)
	}
	var body []byte
	for b := range s.subs {
		if b.c == nil { // Get's pseudo-subscription
			continue
		}
		if b.periodNs > 0 && b.due != 0 && t < b.due-tol {
			continue
		}
		if b.periodNs > 0 {
			if b.due == 0 {
				b.due = t
			}
			b.due += b.periodNs
			if b.due <= t-tol {
				b.due = t + b.periodNs // fell behind by more than a period: resync
			}
		}
		if body == nil {
			body = eventBody(t, v, s.info.Precise)
		}
		line := make([]byte, 0, len(b.prefix)+len(body))
		line = append(append(line, b.prefix...), body...)
		if !b.c.Send(line) {
			h.dropped++
		}
	}
}

// rearm schedules re-enabling a one-shot sensor that just fired, if anyone is
// still subscribed. Called with h.mu held.
func (h *Hub) rearm(s *sensor) {
	s.periodUs = -1 // Android has disabled it
	s.hasLast = false
	h.setAwake(s, false)
	time.AfterFunc(h.RearmDelay, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if s.periodUs < 0 {
			h.reconcile(s)
		}
	})
}

// Get returns the latest reading of the sensor named by spec. If the sensor
// is off, Get turns it on at hz (0 = up to 50 Hz) and waits for the first
// reading. The sensor stays on for Linger after the last Get.
func (h *Hub) Get(spec string, hz float64) (Info, int64, []float64, error) {
	if hz < 0 || math.IsNaN(hz) || math.IsInf(hz, 0) {
		return Info{}, 0, nil, fmt.Errorf("invalid hz %v", hz)
	}
	h.mu.Lock()
	s := h.resolve(spec)
	if s == nil {
		h.mu.Unlock()
		return Info{}, 0, nil, ErrUnknownSensor
	}
	if s.info.Mode == OneShot {
		h.mu.Unlock()
		return Info{}, 0, nil, errors.New("one-shot sensors have no current value; subscribe instead")
	}
	if hz == 0 {
		hz = 50
	}
	regUs := int32(max(1e6/hz, float64(s.info.MinDelayUs)))
	if s.poll == nil {
		s.poll = &sub{s: s, regUs: regUs}
		s.subs[s.poll] = struct{}{}
	} else if regUs < s.poll.regUs {
		s.poll.regUs = regUs
	}
	if err := h.reconcile(s); err != nil {
		delete(s.subs, s.poll)
		s.poll = nil
		h.mu.Unlock()
		return Info{}, 0, nil, err
	}
	s.pollUntil = time.Now().Add(h.Linger)
	if s.pollTimer == nil {
		s.pollTimer = time.AfterFunc(h.Linger, func() { h.endPoll(s) })
	}

	if !s.hasLast {
		w := make(chan struct{})
		s.waiters = append(s.waiters, w)
		h.mu.Unlock()
		select {
		case <-w:
		case <-time.After(h.GetTimeout):
			return Info{}, 0, nil, errors.New("no reading yet (on-change sensors report only when their value changes)")
		}
		h.mu.Lock()
	}
	defer h.mu.Unlock()
	if !s.hasLast {
		return Info{}, 0, nil, errors.New("sensor stopped before its first reading")
	}
	return s.info, s.lastT, append([]float64(nil), s.lastV...), nil
}

// endPoll powers the sensor down once Linger has passed since the last Get.
func (h *Hub) endPoll(s *sensor) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if d := time.Until(s.pollUntil); d > 0 {
		s.pollTimer = time.AfterFunc(d, func() { h.endPoll(s) })
		return
	}
	s.pollTimer = nil
	if s.poll != nil {
		delete(s.subs, s.poll)
		s.poll = nil
		h.reconcile(s)
	}
}

// Active describes a sensor that is currently enabled.
type Active struct {
	Info       Info
	PeriodUs   int32   // registered period
	MeasuredHz float64 // actual event rate; 0 until measured
	Subs       int
}

// Active lists the enabled sensors.
func (h *Hub) Active() []Active {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Active
	for _, s := range h.sensors {
		if s.periodUs < 0 {
			continue
		}
		a := Active{Info: s.info, PeriodUs: s.periodUs, Subs: len(s.subs)}
		if s.avgDt > 0 {
			a.MeasuredHz = 1e9 / float64(s.avgDt)
		}
		out = append(out, a)
	}
	return out
}

// Dropped counts event lines that clients were too slow to take.
func (h *Hub) Dropped() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dropped
}

func eventBody(t int64, v []float64, precise bool) []byte {
	bits := 32
	if precise {
		bits = 64
	}
	b := make([]byte, 0, 32+len(v)*12)
	b = append(b, `"t":`...)
	b = strconv.AppendInt(b, t, 10)
	b = append(b, `,"v":[`...)
	for i, x := range v {
		if i > 0 {
			b = append(b, ',')
		}
		if math.IsNaN(x) || math.IsInf(x, 0) {
			b = append(b, "null"...)
			continue
		}
		b = strconv.AppendFloat(b, x, 'g', -1, bits)
	}
	return append(b, "]}\n"...)
}

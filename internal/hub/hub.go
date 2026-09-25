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
}

func New(be Backend) *Hub {
	h := &Hub{
		be:       be,
		byHandle: map[int32]*sensor{},
		byName:   map[string]*sensor{},
		byType:   map[string]*sensor{},
		clients:  map[Client]map[int64]*sub{},
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
	if s.info.Mode == OneShot {
		return Info{}, 0, errors.New("one-shot sensors are not supported yet")
	}
	if h.clients[c][id] != nil {
		return Info{}, 0, fmt.Errorf("id %d already in use", id)
	}

	b := &sub{c: c, id: id, s: s}
	b.prefix = strconv.AppendInt([]byte(`{"id":`), id, 10)
	b.prefix = append(b.prefix, ',')
	granted := 0.0
	min := s.info.MinDelayUs
	switch {
	case hz == 0:
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
	var body []byte
	for b := range s.subs {
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
			body = eventBody(t, v)
		}
		line := make([]byte, 0, len(b.prefix)+len(body))
		line = append(append(line, b.prefix...), body...)
		if !b.c.Send(line) {
			h.dropped++
		}
	}
}

// Dropped counts event lines that clients were too slow to take.
func (h *Hub) Dropped() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dropped
}

func eventBody(t int64, v []float64) []byte {
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
		b = strconv.AppendFloat(b, x, 'g', -1, 32)
	}
	return append(b, "]}\n"...)
}

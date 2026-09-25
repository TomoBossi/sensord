// Package client connects to the sensord app and streams sensor events.
//
//	c, err := client.Dial("")
//	sub, err := c.Subscribe("accelerometer", 50)
//	for ev := range sub.C {
//		fmt.Println(ev.T, ev.V)
//	}
package client

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/TomoBossi/sensord/proto"
)

// subState is the reader's side of a subscription. Closing stop tells the
// reader to discard further events instead of blocking on ch, so Close can't
// deadlock against a consumer that has stopped reading.
type subState struct {
	ch   chan Event
	stop chan struct{}
}

// Event is one sensor reading.
type Event struct {
	T int64     // ns, CLOCK_BOOTTIME (Android's elapsedRealtimeNanos)
	V []float64 // raw values, as documented for the Android sensor type
}

type Client struct {
	nc net.Conn

	wmu sync.Mutex // serializes writes
	enc *json.Encoder

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan proto.Message
	subs    map[int64]*subState
	err     error // set once the connection is dead
	closed  chan struct{}
}

// Dial connects to sensord at addr, or at proto.DefaultAddr if addr is "".
func Dial(addr string) (*Client, error) {
	if addr == "" {
		addr = proto.DefaultAddr
	}
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("sensord not reachable at %s (is the app running?): %w", addr, err)
	}
	c := &Client{
		nc:      nc,
		enc:     json.NewEncoder(nc),
		pending: map[int64]chan proto.Message{},
		subs:    map[int64]*subState{},
		closed:  make(chan struct{}),
	}
	go c.read()
	return c, nil
}

// Close ends the connection. The server stops every subscription of this
// client, and powers down sensors nobody else is reading.
func (c *Client) Close() error { return c.nc.Close() }

// Err returns why the connection ended, once it has.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *Client) read() {
	sc := bufio.NewScanner(c.nc)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var err error
	for sc.Scan() {
		var m proto.Message
		if err = json.Unmarshal(sc.Bytes(), &m); err != nil {
			break
		}
		c.mu.Lock()
		if m.Op == "" {
			st := c.subs[m.ID]
			c.mu.Unlock()
			if st != nil {
				select {
				case st.ch <- Event{T: m.T, V: m.V}:
				case <-st.stop:
				}
			}
			continue
		}
		ch := c.pending[m.ID]
		delete(c.pending, m.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- m
		}
	}
	if err == nil {
		err = sc.Err()
	}
	if err == nil {
		err = errors.New("connection closed")
	}
	c.mu.Lock()
	c.err = err
	close(c.closed)
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	for id, st := range c.subs {
		close(st.ch)
		delete(c.subs, id)
	}
	c.mu.Unlock()
	c.nc.Close()
}

// call sends req with a fresh id and waits for the reply. If sub is non-nil
// it is registered under the same id first, so no event can be missed.
func (c *Client) call(req proto.Request, sub *subState) (proto.Message, error) {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return proto.Message{}, c.err
	}
	c.nextID++
	req.ID = c.nextID
	reply := make(chan proto.Message, 1)
	c.pending[req.ID] = reply
	if sub != nil {
		c.subs[req.ID] = sub
	}
	c.mu.Unlock()

	c.wmu.Lock()
	err := c.enc.Encode(req)
	c.wmu.Unlock()
	if err != nil {
		c.nc.Close()
		return proto.Message{}, err
	}
	m, ok := <-reply
	if !ok {
		return proto.Message{}, c.Err()
	}
	if m.Op == proto.OpErr {
		c.mu.Lock()
		if sub != nil && c.subs[req.ID] == sub {
			delete(c.subs, req.ID)
			close(sub.ch)
		}
		c.mu.Unlock()
		return m, errors.New(m.Msg)
	}
	m.ID = req.ID
	return m, nil
}

// Sensors lists every sensor on the device.
func (c *Client) Sensors() ([]proto.Sensor, error) {
	m, err := c.call(proto.Request{Op: proto.OpList}, nil)
	return m.Sensors, err
}

// Status reports the server's connections and the sensors currently powered.
func (c *Client) Status() (proto.Status, error) {
	m, err := c.call(proto.Request{Op: proto.OpStatus}, nil)
	if err != nil || m.Status == nil {
		return proto.Status{}, err
	}
	return *m.Status, nil
}

// Ping checks that the server is responsive.
func (c *Client) Ping() error {
	_, err := c.call(proto.Request{Op: proto.OpPing}, nil)
	return err
}

// Subscription is a live stream of events. C is closed when the subscription
// or the connection ends.
type Subscription struct {
	C      <-chan Event
	Sensor string  // resolved sensor name
	Hz     float64 // granted delivery rate; 0 means every event

	id   int64
	c    *Client
	st   *subState
	once sync.Once
}

// Subscribe streams events of sensor (an exact name from Sensors, or a type
// such as "accelerometer", "gyroscope", "light") at hz; 0 means every event.
//
// Read C promptly: if the client falls about 2.5 s behind, the server drops
// events for it rather than stall other clients.
func (c *Client) Subscribe(sensor string, hz float64) (*Subscription, error) {
	st := &subState{ch: make(chan Event, 256), stop: make(chan struct{})}
	m, err := c.call(proto.Request{Op: proto.OpSub, Sensor: sensor, Hz: hz}, st)
	if err != nil {
		return nil, err
	}
	return &Subscription{C: st.ch, Sensor: m.Sensor, Hz: m.Hz, id: m.ID, c: c, st: st}, nil
}

// Close stops the subscription; the sensor powers down if nobody else is
// reading it.
func (s *Subscription) Close() error {
	var err error
	s.once.Do(func() {
		close(s.st.stop)
		_, err = s.c.call(proto.Request{Op: proto.OpUnsub, Sub: s.id}, nil)
		// The server sends every event of this subscription before the unsub
		// reply, and the reader handles lines in order, so no send to ch can
		// still be in flight here.
		c := s.c
		c.mu.Lock()
		if c.subs[s.id] == s.st {
			delete(c.subs, s.id)
			close(s.st.ch)
		}
		c.mu.Unlock()
	})
	return err
}

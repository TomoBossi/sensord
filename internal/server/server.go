// Package server speaks the sensord protocol (see package proto) over TCP on
// top of a hub.
package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"math"
	"net"
	"strconv"
	"sync/atomic"

	"github.com/TomoBossi/sensord/internal/hub"
	"github.com/TomoBossi/sensord/proto"
)

// outBuf is how many lines a client may fall behind before events are dropped.
// About 2.5 s at the maximum rate of 400 Hz.
const outBuf = 1024

type Server struct {
	Hub  *hub.Hub
	Logf func(format string, args ...any)

	conns atomic.Int64
}

// Status reports the connections and the sensors currently powered.
func (s *Server) Status() proto.Status {
	st := proto.Status{Connections: int(s.conns.Load()), Dropped: s.Hub.Dropped(), Active: []proto.ActiveSensor{}}
	for _, a := range s.Hub.Active() {
		as := proto.ActiveSensor{Name: a.Info.Name, Mode: a.Info.Mode.String(), MeasuredHz: a.MeasuredHz, Subscribers: a.Subs}
		if a.PeriodUs > 0 && a.Info.Mode == hub.Continuous {
			as.Hz = 1e6 / float64(a.PeriodUs)
		}
		st.Active = append(st.Active, as)
	}
	return st
}

// Serve accepts connections until ln fails.
func (s *Server) Serve(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		go s.handle(c)
	}
}

// conn is a hub.Client. Events and replies share one ordered queue.
type conn struct {
	out chan []byte
}

func (c *conn) Send(line []byte) bool {
	select {
	case c.out <- line:
		return true
	default:
		return false
	}
}

// reply queues a control message. Unlike events it is never dropped: it
// blocks until the writer takes it, and the reader stalling is the right
// backpressure for a client that sends requests but doesn't read.
func (c *conn) reply(m proto.Message) {
	b, _ := json.Marshal(m)
	c.out <- append(b, '\n')
}

func (s *Server) handle(nc net.Conn) {
	s.conns.Add(1)
	defer s.conns.Add(-1)
	c := &conn{out: make(chan []byte, outBuf)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		w := bufio.NewWriter(nc)
		for line := range c.out {
			if _, err := w.Write(line); err != nil {
				nc.Close() // unblocks the reader, which cleans up
				break
			}
			if len(c.out) == 0 {
				if err := w.Flush(); err != nil {
					nc.Close()
					break
				}
			}
		}
		for range c.out { // drain until the reader closes the queue
		}
	}()

	sc := bufio.NewScanner(nc)
	for sc.Scan() {
		var req proto.Request
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			c.reply(proto.Message{Op: proto.OpErr, Msg: "bad request: " + err.Error()})
			continue
		}
		c.reply(s.do(c, req))
	}

	// Connection gone, whether closed, reset or the process died: stop its
	// sensors before anything else.
	s.Hub.Drop(c)
	close(c.out)
	<-done
	nc.Close()
}

func (s *Server) do(c *conn, req proto.Request) proto.Message {
	fail := func(err error) proto.Message {
		return proto.Message{Op: proto.OpErr, ID: req.ID, Msg: err.Error()}
	}
	switch req.Op {
	case proto.OpPing:
		return proto.Message{Op: proto.OpPong, ID: req.ID}
	case proto.OpStatus:
		st := s.Status()
		return proto.Message{Op: proto.OpStatus, ID: req.ID, Status: &st}
	case proto.OpList:
		infos := s.Hub.Sensors()
		out := make([]proto.Sensor, len(infos))
		for i, in := range infos {
			out[i] = proto.Sensor{
				Name: in.Name, Vendor: in.Vendor, Type: in.Type, TypeID: int(in.TypeID),
				MaxHz: in.MaxHz(), Mode: in.Mode.String(), Wakeup: in.Wakeup, Default: in.Default,
			}
		}
		return proto.Message{Op: proto.OpList, ID: req.ID, Sensors: out}
	case proto.OpSub:
		if req.ID == 0 {
			return fail(errors.New("sub needs a nonzero id"))
		}
		info, hz, err := s.Hub.Subscribe(c, req.ID, req.Sensor, req.Hz)
		if err != nil {
			return fail(err)
		}
		if s.Logf != nil {
			s.Logf("sub %d %s at %.1f Hz", req.ID, info.Name, hz)
		}
		return proto.Message{Op: proto.OpOK, ID: req.ID, Sensor: info.Name, Hz: hz}
	case proto.OpGet:
		info, t, v, err := s.Hub.Get(req.Sensor, req.Hz)
		if err != nil {
			return fail(err)
		}
		// Readings are float32 at the source; print them as such, like the
		// event stream does, instead of their float64 expansion.
		for i, x := range v {
			if !info.Precise && !math.IsNaN(x) && !math.IsInf(x, 0) { // missing values go out as null
				v[i], _ = strconv.ParseFloat(strconv.FormatFloat(x, 'g', -1, 32), 64)
			}
		}
		return proto.Message{Op: proto.OpValue, ID: req.ID, Sensor: info.Name, T: t, V: v}
	case proto.OpUnsub:
		if err := s.Hub.Unsubscribe(c, req.Sub); err != nil {
			return fail(err)
		}
		return proto.Message{Op: proto.OpOK, ID: req.ID}
	default:
		return fail(errors.New("unknown op " + req.Op))
	}
}

// Package proto defines the sensord wire protocol: newline-delimited JSON
// over TCP, one object per line in each direction.
//
// Requests carry an optional client-chosen id that the server echoes back in
// the matching reply. For a subscription, the request id also tags every
// event line of that stream. Event lines are the only messages without "op".
package proto

// DefaultAddr is where the sensord app listens. Loopback only.
const DefaultAddr = "127.0.0.1:47474"

// Request ops.
const (
	OpList   = "list"   // -> Message{Op: "list", Sensors: ...}
	OpSub    = "sub"    // Sensor, Hz -> Message{Op: "ok", Sensor, Hz}, then events
	OpUnsub  = "unsub"  // Sub -> Message{Op: "ok"}
	OpGet    = "get"    // Sensor, Hz -> Message{Op: "value", Sensor, T, V}
	OpPing   = "ping"   // -> Message{Op: "pong"}
	OpStatus = "status" // -> Message{Op: "status", Status: ...}
)

// Reply ops.
const (
	OpOK    = "ok"
	OpErr   = "err"
	OpPong  = "pong"
	OpValue = "value"
)

// Request is a client -> server message.
type Request struct {
	Op     string  `json:"op"`
	ID     int64   `json:"id,omitempty"`
	Sensor string  `json:"sensor,omitempty"` // exact sensor name, or a type such as "accelerometer"
	Hz     float64 `json:"hz,omitempty"`     // sub: 0 = every event; get: rate while warm, 0 = up to 50
	Sub    int64   `json:"sub,omitempty"`    // unsub: id of the sub request to stop
}

// Message is a server -> client message: a reply, or an event when Op is "".
type Message struct {
	Op      string   `json:"op,omitempty"`
	ID      int64    `json:"id,omitempty"`
	Msg     string   `json:"msg,omitempty"`
	Sensor  string   `json:"sensor,omitempty"`
	Hz      float64  `json:"hz,omitempty"`
	Sensors []Sensor `json:"sensors,omitempty"`
	Status  *Status  `json:"status,omitempty"`

	// Event fields.
	T int64     `json:"t,omitempty"` // event time, ns, CLOCK_BOOTTIME (elapsedRealtimeNanos)
	V []float64 `json:"v,omitempty"`
}

// Status is a snapshot of the server's state.
type Status struct {
	Connections int            `json:"connections"`
	Active      []ActiveSensor `json:"active"`
	Dropped     uint64         `json:"dropped"` // events dropped for slow clients, since start
}

// ActiveSensor is a sensor that is currently powered.
type ActiveSensor struct {
	Name        string  `json:"name"`
	Mode        string  `json:"mode"`
	Hz          float64 `json:"hz"`          // registered rate; 0 unless continuous
	MeasuredHz  float64 `json:"measured_hz"` // actual event rate
	Subscribers int     `json:"subscribers"`
}

// Sensor describes one sensor in a list reply.
type Sensor struct {
	Name    string  `json:"name"`
	Vendor  string  `json:"vendor"`
	Type    string  `json:"type"` // "accelerometer" for android.sensor.*, full string for vendor types
	TypeID  int     `json:"type_id"`
	MaxHz   float64 `json:"max_hz,omitempty"` // 0 for on-change and one-shot sensors
	Mode    string  `json:"mode"`             // continuous, on-change, one-shot, special
	Wakeup  bool    `json:"wakeup,omitempty"`
	Default bool    `json:"default,omitempty"` // the sensor a request by type resolves to
}

package proto

import (
	"encoding/json"
	"math"
	"testing"
)

func TestValuesNullIsNaN(t *testing.T) {
	var m Message
	if err := json.Unmarshal([]byte(`{"t":1,"v":[1.5,null,-2]}`), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.V) != 3 || m.V[0] != 1.5 || !math.IsNaN(m.V[1]) || m.V[2] != -2 {
		t.Fatalf("got %v", m.V)
	}
	b, err := json.Marshal(m)
	if err != nil || string(b) != `{"t":1,"v":[1.5,null,-2]}` {
		t.Fatalf("got %s, %v", b, err)
	}
}

package server_test

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/mikefsq/goindi/client"
	"github.com/mikefsq/goindi/server"
)

type focuser interface {
	MoveTo(position int) error
	Position() int
}

type fakeFocuser struct {
	mu  sync.Mutex
	pos int
}

func (f *fakeFocuser) MoveTo(p int) error { f.mu.Lock(); f.pos = p; f.mu.Unlock(); return nil }
func (f *fakeFocuser) Position() int      { f.mu.Lock(); defer f.mu.Unlock(); return f.pos }

type focuserDevice struct {
	name string
	hw   focuser

	conn *server.Property
	info *server.Property
	abs  *server.Property
}

func newFocuserDevice(name string, hw focuser) *focuserDevice {
	d := &focuserDevice{name: name, hw: hw}
	// Every device needs CONNECTION and DRIVER_INFO, whose DRIVER_INTERFACE
	// bitmask tells a client what kind of device this is.
	d.conn = server.ConnectionProperty(name)
	d.info = server.DriverInfoProperty(name, name, "example-focuser", "1.0", server.InterfaceFocuser)
	d.abs = server.NewProperty(name, "ABS_FOCUS_POSITION", server.NumberType, server.RW,
		&server.Member{Name: "FOCUS_ABSOLUTE_POSITION", Label: "Ticks",
			Format: "%.0f", Min: 0, Max: 100000, Num: float64(hw.Position())})
	d.abs.Label, d.abs.Group = "Absolute Position", "Main Control"
	d.abs.SetState(server.Ok)
	return d
}

func (d *focuserDevice) Name() string { return d.name }

func (d *focuserDevice) Properties() []*server.Property {
	return []*server.Property{d.conn, d.info, d.abs}
}

// HandleNew runs on the connection's read loop, so anything slow belongs in a
// goroutine.
func (d *focuserDevice) HandleNew(pub server.Publisher, name string, members []server.NewMember) {
	switch name {
	case "CONNECTION":
		for _, m := range members {
			if m.Name == "CONNECT" {
				d.conn.SetSwitch("CONNECT", m.On())
				d.conn.SetSwitch("DISCONNECT", !m.On())
			}
		}
		d.conn.SetState(server.Ok)
		pub.Update(d.conn)
	case "ABS_FOCUS_POSITION":
		for _, m := range members {
			if m.Name != "FOCUS_ABSOLUTE_POSITION" {
				continue
			}
			pos, ok := m.Float()
			if !ok { // malformed input: refuse rather than move to 0
				d.abs.SetState(server.Alert)
				pub.Update(d.abs)
				continue
			}
			// Busy is how INDI expresses progress; there is no is-moving property.
			d.abs.SetState(server.Busy)
			pub.Update(d.abs)
			_ = d.hw.MoveTo(int(pos))
			d.abs.SetNumber("FOCUS_ABSOLUTE_POSITION", float64(d.hw.Position()))
			d.abs.SetState(server.Ok)
			pub.Update(d.abs)
		}
	}
}

// ExampleDevice serves a minimal focuser device and drives it with a client.
func ExampleDevice() {
	s := server.New("127.0.0.1:0")
	if err := s.AddDevice(newFocuserDevice("Example Focuser", &fakeFocuser{pos: 1000})); err != nil {
		panic(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()
	for s.Addr() == nil {
		time.Sleep(5 * time.Millisecond)
	}

	c, err := client.Dial(ctx, s.Addr().String())
	if err != nil {
		panic(err)
	}
	defer c.Close()
	_ = c.GetProperties("", "")
	c.WaitDevices(1, 2*time.Second)

	_ = c.SetNumber("Example Focuser", "ABS_FOCUS_POSITION",
		map[string]float64{"FOCUS_ABSOLUTE_POSITION": 4200})
	p, _ := c.Wait("Example Focuser", "ABS_FOCUS_POSITION", func(p client.Property) bool {
		m, _ := p.Member("FOCUS_ABSOLUTE_POSITION")
		return m.Num == 4200
	}, 2*time.Second)
	m, _ := p.Member("FOCUS_ABSOLUTE_POSITION")
	fmt.Printf("%s = %.0f\n", p.Name, m.Num)

	// Output:
	// ABS_FOCUS_POSITION = 4200
}

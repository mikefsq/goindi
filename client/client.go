// Package client is a Go INDI client: it connects to an INDI server, enumerates
// its devices and properties, tracks updates, and sets properties. It underpins
// the conform package (the ConformU analogue for INDI) and can also drive any
// INDI server directly. It shares no code with goindi/server, so it validates the
// wire protocol independently rather than round-tripping one codec.
package client

import (
	"context"
	"encoding/xml"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"
)

// Member is one element of a property as seen by the client.
type Member struct {
	Name, Label    string
	Value          string // raw chardata
	Num            float64
	On             bool
	Format         string
	Min, Max, Step float64
}

// Property is the client's view of an INDI property vector.
type Property struct {
	Device, Name, Label, Group string
	Type, State, Perm, Rule    string
	Members                    []Member
}

// Member returns the named member.
func (p Property) Member(name string) (Member, bool) {
	for _, m := range p.Members {
		if m.Name == name {
			return m, true
		}
	}
	return Member{}, false
}

// Client is a connected INDI client. Property reads return copies, safe to use
// while the background read loop applies updates.
type Client struct {
	conn net.Conn

	mu       sync.Mutex
	store    map[string]map[string]*Property
	order    []string
	messages []string
	closed   bool
	err      error
}

// Dial connects to an INDI server at addr ("host:port") and starts reading.
func Dial(ctx context.Context, addr string) (*Client, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("indi client: dial %s: %w", addr, err)
	}
	c := &Client{conn: conn, store: map[string]map[string]*Property{}}
	go c.readLoop()
	return c, nil
}

func (c *Client) Close() error { return c.conn.Close() }

func (c *Client) send(v any) error {
	b, err := xml.Marshal(v)
	if err != nil {
		return err
	}
	_, err = c.conn.Write(append(b, '\n'))
	return err
}

// GetProperties asks the server to define properties (device/name empty = all).
func (c *Client) GetProperties(device, name string) error {
	return c.send(wGetProperties{Version: "1.7", Device: device, Name: name})
}

// SetSwitch sends a newSwitchVector setting the named members On/Off.
func (c *Client) SetSwitch(device, name string, states map[string]bool) error {
	v := wNewVector{XMLName: xml.Name{Local: "newSwitchVector"}, Device: device, Name: name}
	for n, on := range states {
		v.Switches = append(v.Switches, wOne{Name: n, Value: onoff(on)})
	}
	return c.send(v)
}

// SetNumber sends a newNumberVector setting the named members.
func (c *Client) SetNumber(device, name string, vals map[string]float64) error {
	v := wNewVector{XMLName: xml.Name{Local: "newNumberVector"}, Device: device, Name: name}
	for n, val := range vals {
		v.Numbers = append(v.Numbers, wOne{Name: n, Value: strconv.FormatFloat(val, 'f', 6, 64)})
	}
	return c.send(v)
}

// SetText sends a newTextVector setting the named members.
func (c *Client) SetText(device, name string, vals map[string]string) error {
	v := wNewVector{XMLName: xml.Name{Local: "newTextVector"}, Device: device, Name: name}
	for n, val := range vals {
		v.Texts = append(v.Texts, wOne{Name: n, Value: val})
	}
	return c.send(v)
}

func (c *Client) readLoop() {
	dec := xml.NewDecoder(c.conn)
	for {
		tok, err := dec.Token()
		if err != nil {
			c.mu.Lock()
			c.closed, c.err = true, err
			c.mu.Unlock()
			return
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "defNumberVector", "defSwitchVector", "defTextVector", "defLightVector", "defBLOBVector":
			var v wDefVector
			if dec.DecodeElement(&v, &se) == nil {
				c.applyDef(se.Name.Local, v)
			}
		case "setNumberVector", "setSwitchVector", "setTextVector", "setLightVector", "setBLOBVector":
			var v wSetVector
			if dec.DecodeElement(&v, &se) == nil {
				c.applySet(v)
			}
		case "delProperty":
			var v wDelProperty
			if dec.DecodeElement(&v, &se) == nil {
				c.applyDel(v)
			}
		case "message":
			var v wMessage
			if dec.DecodeElement(&v, &se) == nil && v.Message != "" {
				c.mu.Lock()
				c.messages = append(c.messages, v.Message)
				c.mu.Unlock()
			}
		default:
			_ = dec.Skip()
		}
	}
}

func (c *Client) applyDef(elem string, v wDefVector) {
	p := &Property{
		Device: v.Device, Name: v.Name, Label: v.Label, Group: v.Group,
		Type: typeOf(elem), State: v.State, Perm: v.Perm, Rule: v.Rule,
	}
	for _, n := range v.Numbers {
		p.Members = append(p.Members, Member{
			Name: n.Name, Label: n.Label, Value: n.Value, Num: atof(n.Value),
			Format: n.Format, Min: atof(n.Min), Max: atof(n.Max), Step: atof(n.Step),
		})
	}
	for _, s := range v.Switches {
		p.Members = append(p.Members, Member{Name: s.Name, Label: s.Label, Value: s.Value, On: isOn(s.Value)})
	}
	for _, t := range v.Texts {
		p.Members = append(p.Members, Member{Name: t.Name, Label: t.Label, Value: t.Value})
	}
	for _, l := range v.Lights {
		p.Members = append(p.Members, Member{Name: l.Name, Label: l.Label, Value: l.Value})
	}
	c.mu.Lock()
	if c.store[v.Device] == nil {
		c.store[v.Device] = map[string]*Property{}
		c.order = append(c.order, v.Device)
	}
	c.store[v.Device][v.Name] = p
	c.mu.Unlock()
}

func (c *Client) applySet(v wSetVector) {
	c.mu.Lock()
	defer c.mu.Unlock()
	dm := c.store[v.Device]
	if dm == nil {
		return
	}
	p := dm[v.Name]
	if p == nil {
		return
	}
	if v.State != "" {
		p.State = v.State
	}
	upd := func(name, val string) {
		for i := range p.Members {
			if p.Members[i].Name == name {
				p.Members[i].Value = val
				p.Members[i].Num = atof(val)
				p.Members[i].On = isOn(val)
			}
		}
	}
	for _, n := range v.Numbers {
		upd(n.Name, n.Value)
	}
	for _, s := range v.Switches {
		upd(s.Name, s.Value)
	}
	for _, t := range v.Texts {
		upd(t.Name, t.Value)
	}
}

func (c *Client) applyDel(v wDelProperty) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if dm := c.store[v.Device]; dm != nil {
		if v.Name == "" {
			delete(c.store, v.Device)
		} else {
			delete(dm, v.Name)
		}
	}
}

func clone(p *Property) Property {
	cp := *p
	cp.Members = append([]Member(nil), p.Members...)
	return cp
}

// Devices returns the device names discovered so far, in arrival order.
func (c *Client) Devices() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.order...)
}

// Property returns a copy of the named property, if known.
func (c *Client) Property(device, name string) (Property, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if dm := c.store[device]; dm != nil {
		if p := dm[name]; p != nil {
			return clone(p), true
		}
	}
	return Property{}, false
}

// Properties returns copies of all known properties for a device.
func (c *Client) Properties(device string) []Property {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Property
	if dm := c.store[device]; dm != nil {
		for _, p := range dm {
			out = append(out, clone(p))
		}
	}
	return out
}

// Messages returns the <message> log lines received so far.
func (c *Client) Messages() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.messages...)
}

func (c *Client) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// Wait polls until the named property satisfies pred or timeout elapses.
func (c *Client) Wait(device, name string, pred func(Property) bool, timeout time.Duration) (Property, bool) {
	deadline := time.Now().Add(timeout)
	for {
		if p, ok := c.Property(device, name); ok && pred(p) {
			return p, true
		}
		if c.isClosed() || !time.Now().Before(deadline) {
			p, _ := c.Property(device, name)
			return p, false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// WaitDevices polls until at least n devices are known or timeout elapses.
func (c *Client) WaitDevices(n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(c.Devices()) >= n {
			return true
		}
		if c.isClosed() {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return len(c.Devices()) >= n
}

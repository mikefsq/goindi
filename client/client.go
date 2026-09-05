// Package client is a Go INDI client: it connects to an INDI server, enumerates
// its devices and properties, tracks updates, and sets properties.
package client

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
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
	Size           int64 // BLOB members only: the declared uncompressed length
}

// Property is the client's view of an INDI property vector.
type Property struct {
	Device, Name, Label, Group string
	Type, State, Perm, Rule    string

	// Message is the message attr of the last set*Vector that carried one, the
	// device's stated reason when State goes Alert.
	Message string

	// Rev counts updates applied to this property, 1 at definition and +1 for
	// every set or redefinition since.
	Rev uint64

	Members []Member
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

// Message is one <message> log line from the server.
type Message struct {
	Device    string // sending device; empty for server-level messages
	Timestamp string // sender's timestamp attr, verbatim
	Text      string
}

// String renders the message with its device and timestamp, when present.
func (m Message) String() string {
	s := m.Text
	if m.Device != "" {
		s = m.Device + ": " + s
	}
	if m.Timestamp != "" {
		s = m.Timestamp + " " + s
	}
	return s
}

// maxMessages caps the message log, dropping the oldest lines first.
const maxMessages = 1000

// Client is a connected INDI client whose property reads return copies, safe to
// use while the background read loop applies updates.
type Client struct {
	conn net.Conn
	done chan struct{}

	mu        sync.Mutex
	updated   chan struct{} // closed and replaced on every store change; Wait-family waiters listen on it
	blobSink  func(BlobInfo) io.Writer
	blobSinks map[string]func(BlobInfo) io.Writer // per device; takes precedence over blobSink
	store     map[string]map[string]*Property
	order     []string
	messages  []Message // a ring once maxMessages is reached
	msgNext   int
	logf      func(format string, args ...any)
	softErrs  int64
	closed    bool
	err       error
}

// Dial connects to an INDI server at addr ("host:port") and starts reading.
func Dial(ctx context.Context, addr string) (*Client, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("indi client: dial %s: %w", addr, err)
	}
	c := &Client{
		conn:    conn,
		done:    make(chan struct{}),
		updated: make(chan struct{}),
		store:   map[string]map[string]*Property{},
	}
	go c.readLoop()
	return c, nil
}

// Close closes the connection, ending the read loop.
func (c *Client) Close() error { return c.conn.Close() }

// Done returns a channel closed when the read loop exits; the client does not
// reconnect, so a caller that must survive server restarts redials.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err reports the terminal read-loop error, nil while the connection is alive.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		return nil
	}
	return c.err
}

func (c *Client) fail(err error) {
	c.mu.Lock()
	c.closed, c.err = true, err
	c.signalLocked()
	c.mu.Unlock()
	close(c.done)
}

// signalLocked wakes every Wait-family waiter; the caller holds c.mu.
func (c *Client) signalLocked() {
	if c.updated != nil {
		close(c.updated)
		c.updated = make(chan struct{})
	}
}

// writeTimeout bounds each send; a var so tests can shorten it.
var writeTimeout = 10 * time.Second

func (c *Client) send(v any) error {
	b, err := xml.Marshal(v)
	if err != nil {
		return err
	}
	c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	_, err = c.conn.Write(append(b, '\n'))
	c.conn.SetWriteDeadline(time.Time{})
	return err
}

// Logf installs a destination for the client's soft-error trace; fn runs on the
// read loop, so it must not block.
func (c *Client) Logf(fn func(format string, args ...any)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logf = fn
}

// SoftErrors reports how many non-fatal protocol errors the client has swallowed.
func (c *Client) SoftErrors() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.softErrs
}

func (c *Client) softError(format string, args ...any) {
	c.mu.Lock()
	c.softErrs++
	fn := c.logf
	c.mu.Unlock()
	if fn != nil {
		fn(format, args...)
	}
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
			c.fail(err)
			return
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "defNumberVector", "defSwitchVector", "defTextVector", "defLightVector", "defBLOBVector":
			var v wDefVector
			if err := dec.DecodeElement(&v, &se); err == nil {
				c.applyDef(se.Name.Local, v)
			} else {
				c.softError("indi client: dropped malformed %s: %v", se.Name.Local, err)
			}
		case "setBLOBVector":
			// A failure here leaves the stream position unknown, so it ends the
			// connection rather than being swallowed as a soft error.
			if err := c.readBlobVector(dec, se); err != nil {
				c.fail(err)
				return
			}
		case "setNumberVector", "setSwitchVector", "setTextVector", "setLightVector":
			var v wSetVector
			if err := dec.DecodeElement(&v, &se); err == nil {
				c.applySet(v)
			} else {
				c.softError("indi client: dropped malformed %s: %v", se.Name.Local, err)
			}
		case "delProperty":
			var v wDelProperty
			if err := dec.DecodeElement(&v, &se); err == nil {
				c.applyDel(v)
			} else {
				c.softError("indi client: dropped malformed delProperty: %v", err)
			}
		case "message":
			var v wMessage
			if err := dec.DecodeElement(&v, &se); err == nil {
				if v.Message != "" {
					c.addMessage(Message{Device: v.Device, Timestamp: v.Timestamp, Text: v.Message})
				}
			} else {
				c.softError("indi client: dropped malformed message: %v", err)
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
	// Chardata is trimmed once, at ingestion: servers wrap values in newlines
	// and indentation.
	for _, n := range v.Numbers {
		val := strings.TrimSpace(n.Value)
		p.Members = append(p.Members, Member{
			Name: n.Name, Label: n.Label, Value: val, Num: atof(val),
			Format: n.Format, Min: atof(n.Min), Max: atof(n.Max), Step: atof(n.Step),
		})
	}
	for _, s := range v.Switches {
		val := strings.TrimSpace(s.Value)
		p.Members = append(p.Members, Member{Name: s.Name, Label: s.Label, Value: val, On: isOn(val)})
	}
	for _, t := range v.Texts {
		p.Members = append(p.Members, Member{Name: t.Name, Label: t.Label, Value: strings.TrimSpace(t.Value)})
	}
	for _, l := range v.Lights {
		p.Members = append(p.Members, Member{Name: l.Name, Label: l.Label, Value: strings.TrimSpace(l.Value)})
	}
	for _, b := range v.BLOBs {
		p.Members = append(p.Members, Member{Name: b.Name, Label: b.Label})
	}
	c.mu.Lock()
	if c.store[v.Device] == nil {
		c.store[v.Device] = map[string]*Property{}
		if !slices.Contains(c.order, v.Device) { // redefinition must not duplicate
			c.order = append(c.order, v.Device)
		}
	}
	if old := c.store[v.Device][v.Name]; old != nil {
		p.Rev = old.Rev // a redefinition continues the count, keeping it monotonic
	}
	p.Rev++
	c.store[v.Device][v.Name] = p
	c.signalLocked()
	c.mu.Unlock()
}

func (c *Client) addMessage(m Message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.messages) < maxMessages {
		c.messages = append(c.messages, m)
		return
	}
	c.messages[c.msgNext] = m
	c.msgNext = (c.msgNext + 1) % maxMessages
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
	if v.Message != "" {
		p.Message = v.Message
	}
	upd := func(name, val string) {
		val = strings.TrimSpace(val) // the wire pads chardata
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
	for _, l := range v.Lights {
		upd(l.Name, l.Value)
	}
	p.Rev++
	c.signalLocked()
}

func (c *Client) applyDel(v wDelProperty) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if dm := c.store[v.Device]; dm != nil {
		if v.Name == "" {
			delete(c.store, v.Device)
			if i := slices.Index(c.order, v.Device); i >= 0 {
				c.order = slices.Delete(c.order, i, i+1)
			}
		} else {
			delete(dm, v.Name)
		}
		c.signalLocked()
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

// Messages returns the last maxMessages log lines received, oldest first.
func (c *Client) Messages() []Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Message, 0, len(c.messages))
	out = append(out, c.messages[c.msgNext:]...) // msgNext is 0 until the ring fills
	return append(out, c.messages[:c.msgNext]...)
}

// WaitRev blocks until the named property is updated past sinceRev and satisfies
// pred; on timeout or connection death it returns the last-known snapshot and an
// error.
func (c *Client) WaitRev(device, name string, sinceRev uint64, pred func(Property) bool, timeout time.Duration) (Property, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return c.WaitRevCtx(ctx, device, name, sinceRev, pred)
}

// WaitRevCtx is WaitRev bounded by ctx. Cancellation wraps context.Canceled;
// a deadline returns a timeout error. Neither cancels a device operation.
func (c *Client) WaitRevCtx(ctx context.Context, device, name string, sinceRev uint64, pred func(Property) bool) (Property, error) {
	for {
		c.mu.Lock()
		ch := c.updated
		dead, derr := c.closed, c.err
		var snap Property
		if dm := c.store[device]; dm != nil {
			if p := dm[name]; p != nil {
				snap = clone(p)
			}
		}
		c.mu.Unlock()
		if snap.Rev > sinceRev && pred(snap) {
			return snap, nil
		}
		if dead {
			return snap, fmt.Errorf("indi client: connection down waiting for %s.%s: %w", device, name, derr)
		}
		select {
		case <-ch:
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return snap, fmt.Errorf("indi client: timeout waiting for %s.%s", device, name)
			}
			return snap, fmt.Errorf("indi client: waiting for %s.%s: %w", device, name, ctx.Err())
		}
	}
}

// Wait polls until the named property satisfies pred, returning (last-known
// state, false) on timeout or connection death; pred may be satisfied by cached
// pre-command state, so use WaitRev to await an acknowledgement.
func (c *Client) Wait(device, name string, pred func(Property) bool, timeout time.Duration) (Property, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return c.WaitCtx(ctx, device, name, pred)
}

// WaitCtx is Wait bounded by ctx. Cached state can satisfy the predicate;
// use WaitRevCtx when an update is required.
func (c *Client) WaitCtx(ctx context.Context, device, name string, pred func(Property) bool) (Property, bool) {
	t := time.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	for {
		if p, ok := c.Property(device, name); ok && pred(p) {
			return p, true
		}
		if c.isClosed() {
			p, _ := c.Property(device, name)
			return p, false
		}
		select {
		case <-t.C:
		case <-ctx.Done():
			p, _ := c.Property(device, name)
			return p, false
		}
	}
}

// WaitDevices polls until at least n devices are known, returning early if the
// connection dies.
func (c *Client) WaitDevices(n int, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return c.WaitDevicesCtx(ctx, n)
}

// WaitDevicesCtx is WaitDevices bounded by a context.
func (c *Client) WaitDevicesCtx(ctx context.Context, n int) bool {
	t := time.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	for {
		if len(c.Devices()) >= n {
			return true
		}
		if c.isClosed() {
			return false
		}
		select {
		case <-t.C:
		case <-ctx.Done():
			return len(c.Devices()) >= n // a device may have arrived since the last poll
		}
	}
}

func (c *Client) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// SetNumberAndWait sends a vector and waits for a later update in Ok or Alert.
// It returns an error for Alert. INDI updates have no command IDs, so unrelated
// updates can satisfy the wait. Use WaitRev for other completion states.
func (c *Client) SetNumberAndWait(device, name string, vals map[string]float64, timeout time.Duration) (Property, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return c.SetNumberAndWaitCtx(ctx, device, name, vals)
}

// SetNumberAndWaitCtx bounds the acknowledgement wait with ctx.
// The send uses its own write timeout; cancellation does not stop device activity.
func (c *Client) SetNumberAndWaitCtx(ctx context.Context, device, name string, vals map[string]float64) (Property, error) {
	return c.setAndWaitCtx(ctx, device, name, func() error { return c.SetNumber(device, name, vals) })
}

// SetSwitchAndWait is SetSwitch with SetNumberAndWait's acknowledgement wait.
func (c *Client) SetSwitchAndWait(device, name string, states map[string]bool, timeout time.Duration) (Property, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return c.SetSwitchAndWaitCtx(ctx, device, name, states)
}

// SetSwitchAndWaitCtx is SetSwitchAndWait bounded by a context.
func (c *Client) SetSwitchAndWaitCtx(ctx context.Context, device, name string, states map[string]bool) (Property, error) {
	return c.setAndWaitCtx(ctx, device, name, func() error { return c.SetSwitch(device, name, states) })
}

// SetTextAndWait is SetText with SetNumberAndWait's acknowledgement wait.
func (c *Client) SetTextAndWait(device, name string, vals map[string]string, timeout time.Duration) (Property, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return c.SetTextAndWaitCtx(ctx, device, name, vals)
}

// SetTextAndWaitCtx is SetTextAndWait bounded by a context.
func (c *Client) SetTextAndWaitCtx(ctx context.Context, device, name string, vals map[string]string) (Property, error) {
	return c.setAndWaitCtx(ctx, device, name, func() error { return c.SetText(device, name, vals) })
}

func (c *Client) setAndWaitCtx(ctx context.Context, device, name string, send func() error) (Property, error) {
	var since uint64
	if p, ok := c.Property(device, name); ok {
		// Recorded before the command, so only a later update can satisfy the wait.
		since = p.Rev
	}
	if err := send(); err != nil {
		return Property{}, err
	}
	p, err := c.WaitRevCtx(ctx, device, name, since, func(p Property) bool {
		return p.State == "Ok" || p.State == "Alert"
	})
	if err != nil {
		return p, err
	}
	if p.State == "Alert" {
		if p.Message != "" {
			return p, fmt.Errorf("indi client: %s.%s: Alert: %s", device, name, p.Message)
		}
		return p, fmt.Errorf("indi client: %s.%s: Alert", device, name)
	}
	return p, nil
}

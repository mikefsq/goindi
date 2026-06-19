package server

import (
	"context"
	"encoding/xml"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Server is an INDI hub: it listens on one TCP port and multiplexes any number of
// registered Devices to any number of clients, routing each inbound message by its
// device attribute and broadcasting every property change to all clients. This is
// what indiserver does, in-process — no driver binaries, no internal channel.
//
// Register devices with AddDevice before Serve. *Server implements Publisher, so it
// is what each device's handlers and poll loop publish through.
type Server struct {
	addrs []string // one or more "host:port" the hub listens on
	logf  func(string, ...any)

	mu      sync.Mutex
	devices map[string]Device
	order   []Device
	conns   map[*conn]struct{}
	lns     []net.Listener
}

// conn is one client connection with a serialized writer. blobs records whether the
// client enabled BLOB delivery (INDI sends BLOBs only after an enableBLOB request).
type conn struct {
	nc    net.Conn
	wmu   sync.Mutex
	blobs atomic.Bool
}

func (c *conn) write(b []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := c.nc.Write(b)
	return err
}

// id is a short client identifier for traffic logs (the remote address).
func (c *conn) id() string {
	if c.nc != nil {
		return c.nc.RemoteAddr().String()
	}
	return "?"
}

// newMembersStr renders a new*Vector's members as "name=value ..." for logging.
func newMembersStr(v xnewVector) string {
	var b strings.Builder
	for _, n := range v.Numbers {
		fmt.Fprintf(&b, " %s=%s", n.Name, strings.TrimSpace(n.Value))
	}
	for _, sw := range v.Switches {
		fmt.Fprintf(&b, " %s=%s", sw.Name, strings.TrimSpace(sw.Value))
	}
	for _, t := range v.Texts {
		fmt.Fprintf(&b, " %s=%s", t.Name, strings.TrimSpace(t.Value))
	}
	return strings.TrimSpace(b.String())
}

// Option configures a Server.
type Option func(*Server)

// WithLogger sets a diagnostics sink (e.g. log.Printf). Nil disables logging.
func WithLogger(f func(string, ...any)) Option { return func(s *Server) { s.logf = f } }

// WithListenAddrs replaces the single New(addr) listener with an explicit set of
// "host:port" addresses (one listener per address, all feeding the one hub). Use it
// to bind specific interface addresses instead of the wildcard.
func WithListenAddrs(addrs ...string) Option {
	return func(s *Server) {
		if len(addrs) > 0 {
			s.addrs = append([]string(nil), addrs...)
		}
	}
}

// New builds a Server that will listen on addr (":7624" by convention). Use
// WithListenAddrs to bind a specific set of addresses instead.
func New(addr string, opts ...Option) *Server {
	s := &Server{addrs: []string{addr}, devices: map[string]Device{}, conns: map[*conn]struct{}{}}
	for _, o := range opts {
		o(s)
	}
	return s
}

// AddDevice registers a device under its Name. A duplicate name is rejected (the
// name is the client's only addressing key, so collisions must not be silent).
func (s *Server) AddDevice(d Device) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.devices[d.Name()]; ok {
		return fmt.Errorf("indi: duplicate device name %q", d.Name())
	}
	s.devices[d.Name()] = d
	s.order = append(s.order, d)
	return nil
}

func (s *Server) log(f string, a ...any) {
	if s.logf != nil {
		s.logf(f, a...)
	}
}

// Serve listens, starts each device's background loop, and accepts clients until
// ctx is cancelled (one goroutine per connection). Returns nil on clean shutdown.
func (s *Server) Serve(ctx context.Context) error {
	var lns []net.Listener
	for _, a := range s.addrs {
		ln, err := net.Listen("tcp", a)
		if err != nil {
			s.log("indi: listen %s failed: %v", a, err)
			continue
		}
		lns = append(lns, ln)
		s.log("indi: listening on %s", ln.Addr())
	}
	if len(lns) == 0 {
		return fmt.Errorf("indi: could not bind any of %v", s.addrs)
	}
	s.mu.Lock()
	s.lns = lns
	s.mu.Unlock()
	go func() {
		<-ctx.Done()
		for _, ln := range lns {
			_ = ln.Close()
		}
	}()

	for _, d := range s.allDevices() {
		if st, ok := d.(Starter); ok {
			go st.Start(ctx, s)
		}
	}

	// One accept loop per listener; all register into the shared hub. A fatal accept
	// error (not shutdown) on any listener ends Serve.
	errc := make(chan error, len(lns))
	for _, ln := range lns {
		go func(ln net.Listener) { errc <- s.acceptLoop(ctx, ln) }(ln)
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		return err
	}
}

// acceptLoop accepts clients on one listener until it is closed or errors.
func (s *Server) acceptLoop(ctx context.Context, ln net.Listener) error {
	for {
		nc, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("indi: accept: %w", err)
		}
		c := &conn{nc: nc}
		s.mu.Lock()
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		s.log("indi: client %s connected", c.id())
		go s.handle(ctx, c)
	}
}

// Addr is the first listening address (valid after Serve has started).
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.lns) == 0 {
		return nil
	}
	return s.lns[0].Addr()
}

func (s *Server) removeConn(c *conn) {
	s.mu.Lock()
	_, existed := s.conns[c]
	delete(s.conns, c)
	s.mu.Unlock()
	if existed {
		s.log("indi: client %s disconnected", c.id())
	}
	_ = c.nc.Close()
}

func (s *Server) device(name string) Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.devices[name]
}

func (s *Server) allDevices() []Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Device(nil), s.order...)
}

// handle runs one connection's read loop. INDI is a stream of top-level elements,
// so we drive xml.Decoder with a Token loop (not Decode, which expects one root).
func (s *Server) handle(ctx context.Context, c *conn) {
	defer s.removeConn(c)
	go func() { <-ctx.Done(); _ = c.nc.Close() }()

	dec := xml.NewDecoder(c.nc)
	for {
		tok, err := dec.Token()
		if err != nil {
			return // peer closed or shutdown
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "getProperties":
			var v xgetProperties
			if dec.DecodeElement(&v, &se) == nil {
				s.log("indi: <- %s getProperties device=%q name=%q", c.id(), v.Device, v.Name)
				s.snapshot(c, v.Device, v.Name)
			}
		case "newNumberVector", "newSwitchVector", "newTextVector":
			var v xnewVector
			if dec.DecodeElement(&v, &se) == nil {
				s.log("indi: <- %s %s %s/%s [%s]", c.id(), se.Name.Local, v.Device, v.Name, newMembersStr(v))
				s.dispatchNew(v)
			}
		case "enableBLOB":
			var v xenableBLOB
			if dec.DecodeElement(&v, &se) == nil {
				// "Never" (the default) suppresses BLOBs; "Also"/"Only" enable them.
				on := !strings.EqualFold(strings.TrimSpace(v.Value), "Never")
				c.blobs.Store(on)
				s.log("indi: <- %s enableBLOB device=%q name=%q value=%q -> blobs=%v", c.id(), v.Device, v.Name, strings.TrimSpace(v.Value), on)
			}
		default:
			_ = dec.Skip()
		}
	}
}

// snapshot sends def*Vectors for the requested device/name (empty = all) to one
// client — the initial property enumeration a client builds its device list from.
func (s *Server) snapshot(c *conn, device, name string) {
	ts := now()
	for _, d := range s.allDevices() {
		if device != "" && d.Name() != device {
			continue
		}
		for _, p := range d.Properties() {
			if name != "" && p.Name != name {
				continue
			}
			if b, err := marshalDef(p, ts); err == nil {
				_ = c.write(frame(b))
			}
		}
	}
}

func (s *Server) dispatchNew(v xnewVector) {
	d := s.device(v.Device)
	if d == nil {
		return
	}
	var members []NewMember
	for _, n := range v.Numbers {
		members = append(members, NewMember{n.Name, n.Value})
	}
	for _, sw := range v.Switches {
		members = append(members, NewMember{sw.Name, sw.Value})
	}
	for _, t := range v.Texts {
		members = append(members, NewMember{t.Name, t.Value})
	}
	d.HandleNew(s, v.Name, members)
}

// ---- Publisher: broadcast to all connected clients ----

func (s *Server) broadcast(b []byte) {
	msg := frame(b)
	s.mu.Lock()
	conns := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		if err := c.write(msg); err != nil {
			s.removeConn(c)
		}
	}
}

func (s *Server) Define(p *Property) {
	if b, err := marshalDef(p, now()); err == nil {
		s.broadcast(b)
	}
}

func (s *Server) Update(p *Property) {
	if b, err := marshalSet(p, now()); err == nil {
		s.broadcast(b)
	}
}

func (s *Server) Message(device, msg string) {
	if b, err := xml.Marshal(xmessage{Device: device, Timestamp: now(), Message: msg}); err == nil {
		s.broadcast(b)
	}
}

func (s *Server) Delete(device, name string) {
	if b, err := xml.Marshal(xdelProperty{Device: device, Name: name, Timestamp: now()}); err == nil {
		s.broadcast(b)
	}
}

// SendBLOB delivers a one-element BLOB (e.g. a CCD frame as FITS) to every client
// that enabled BLOBs via enableBLOB — INDI does not push BLOBs to a client until it
// asks. Data is sent verbatim (no transformation); the CCD device is responsible
// for delivering a raw frame.
func (s *Server) SendBLOB(device, name, elem, format string, data []byte) {
	msg := frame(blobSetXML(device, name, elem, format, data, now()))
	s.mu.Lock()
	total := len(s.conns)
	conns := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		if c.blobs.Load() {
			conns = append(conns, c)
		}
	}
	s.mu.Unlock()
	s.log("indi: -> setBLOBVector %s/%s %d bytes to %d/%d client(s)", device, name, len(data), len(conns), total)
	for _, c := range conns {
		if err := c.write(msg); err != nil {
			s.removeConn(c)
		}
	}
}

// frame appends the inter-element newline INDI peers tolerate between messages.
func frame(b []byte) []byte { return append(b, '\n') }

func now() string { return time.Now().UTC().Format("2006-01-02T15:04:05") }

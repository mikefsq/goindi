package server

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// Server is an INDI hub that multiplexes registered Devices to any number of
// clients over one TCP port.
type Server struct {
	addr  string
	logf  func(string, ...any)
	debug bool

	mu       sync.Mutex
	devices  map[string]Device
	order    []Device
	conns    map[*conn]struct{}
	ln       net.Listener
	serveCtx context.Context // non-nil once Serve is running; AddDevice starts late Starters with it
}

// Per-connection outbound bounds; vars so tests can shrink them.
var (
	outQueueMsgs  = 64
	outQueueBytes = 32 << 20
	writeTimeout  = 10 * time.Second
)

// blobPolicy is one connection's enableBLOB state for a scope; INDI's default is
// Never, so a client that has not asked receives no BLOBs at all.
type blobPolicy uint8

const (
	blobNever blobPolicy = iota
	blobAlso
	blobOnly
)

func parseBlobPolicy(s string) (blobPolicy, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "never":
		return blobNever, true
	case "also":
		return blobAlso, true
	case "only":
		return blobOnly, true
	}
	return 0, false
}

type msgKind uint8

const (
	kindOther msgKind = iota
	kindBLOBDef
	kindBLOBSet
)

type conn struct {
	nc net.Conn

	qmu    sync.Mutex
	queue  chan []byte
	qbytes int
	closed bool
	done   chan struct{}

	pmu sync.Mutex
	// Keyed device+"\x00"+name; an empty name is the device-wide entry and an
	// empty device covers every device.
	policy map[string]blobPolicy
}

func newConn(nc net.Conn) *conn {
	return &conn{nc: nc, queue: make(chan []byte, outQueueMsgs), done: make(chan struct{})}
}

func (c *conn) enqueue(b []byte) (ok, full bool) {
	c.qmu.Lock()
	defer c.qmu.Unlock()
	if c.closed {
		return false, false
	}
	// A message larger than the whole budget is still admitted when the queue is empty.
	if c.qbytes > 0 && c.qbytes+len(b) > outQueueBytes {
		return false, true
	}
	select {
	case c.queue <- b:
		c.qbytes += len(b)
		return true, false
	default:
		return false, true
	}
}

func (c *conn) isClosed() bool {
	c.qmu.Lock()
	defer c.qmu.Unlock()
	return c.closed
}

func (c *conn) setPolicy(device, name string, p blobPolicy) {
	c.pmu.Lock()
	defer c.pmu.Unlock()
	if c.policy == nil {
		c.policy = map[string]blobPolicy{}
	}
	c.policy[device+"\x00"+name] = p
}

func (c *conn) policyFor(device, name string) blobPolicy {
	c.pmu.Lock()
	defer c.pmu.Unlock()
	for _, k := range []string{device + "\x00" + name, device + "\x00", "\x00"} {
		if p, ok := c.policy[k]; ok {
			return p
		}
	}
	return blobNever
}

func (c *conn) wants(device, name string, kind msgKind) bool {
	// defBLOBVector is metadata, not payload: it always passes, or a client could
	// never learn the property exists in order to enable it.
	if kind == kindBLOBDef {
		return true
	}
	p := c.policyFor(device, name)
	if kind == kindBLOBSet {
		return p != blobNever
	}
	return p != blobOnly
}

func (c *conn) id() string {
	if c.nc != nil {
		return c.nc.RemoteAddr().String()
	}
	return "?"
}

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

// WithLogger sets a diagnostics sink; nil disables logging.
func WithLogger(f func(string, ...any)) Option { return func(s *Server) { s.logf = f } }

// WithDebug enables per-message traffic logging.
func WithDebug(on bool) Option { return func(s *Server) { s.debug = on } }

// New builds a Server that will listen on addr (":7624" by convention).
func New(addr string, opts ...Option) *Server {
	s := &Server{addr: addr, devices: map[string]Device{}, conns: map[*conn]struct{}{}}
	for _, o := range opts {
		o(s)
	}
	return s
}

// AddDevice registers a device under its Name, before or after Serve; duplicate
// names are rejected.
func (s *Server) AddDevice(d Device) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.devices[d.Name()]; ok {
		return fmt.Errorf("indi: duplicate device name %q", d.Name())
	}
	s.devices[d.Name()] = d
	s.order = append(s.order, d)
	if s.serveCtx != nil {
		if st, ok := d.(Starter); ok {
			go st.Start(s.serveCtx, s)
		}
	}
	return nil
}

func (s *Server) log(f string, a ...any) {
	if s.logf != nil {
		s.logf(f, a...)
	}
}

func (s *Server) dlog(f string, a ...any) {
	if s.debug {
		s.log(f, a...)
	}
}

// Serve listens, starts each device's background loop, and accepts clients until
// ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("indi: listen %s failed: %w", s.addr, err)
	}
	s.log("indi: listening on %s", ln.Addr())
	// One lock covers serveCtx and the snapshot so a concurrently added device is
	// started exactly once, here or by AddDevice but not both.
	s.mu.Lock()
	s.ln = ln
	s.serveCtx = ctx
	devs := append([]Device(nil), s.order...)
	s.mu.Unlock()
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for _, d := range devs {
		if st, ok := d.(Starter); ok {
			go st.Start(ctx, s)
		}
	}

	return s.acceptLoop(ctx, ln)
}

// acceptLoop retries transient Accept errors with backoff rather than killing the hub.
func (s *Server) acceptLoop(ctx context.Context, ln net.Listener) error {
	var backoff time.Duration
	for {
		nc, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return fmt.Errorf("indi: accept: %w", err)
			}
			switch {
			case backoff == 0:
				backoff = 5 * time.Millisecond
			case backoff < time.Second:
				backoff *= 2
				if backoff > time.Second {
					backoff = time.Second
				}
			}
			s.log("indi: accept: %v; retrying in %v", err, backoff)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			continue
		}
		backoff = 0
		c := newConn(nc)
		s.mu.Lock()
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		s.log("indi: client %s connected", c.id())
		go s.writeLoop(c)
		go s.handle(ctx, c)
	}
}

// Addr is the listening address, valid once Serve has started.
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// removeConn must stay idempotent: the read loop, the writer, and the enqueue
// path all race here.
func (s *Server) removeConn(c *conn) {
	s.mu.Lock()
	_, existed := s.conns[c]
	delete(s.conns, c)
	s.mu.Unlock()
	c.qmu.Lock()
	if !c.closed {
		c.closed = true
		close(c.done)
		close(c.queue)
	}
	c.qmu.Unlock()
	if existed {
		s.log("indi: client %s disconnected", c.id())
	}
	_ = c.nc.Close()
}

func (s *Server) writeLoop(c *conn) {
	for b := range c.queue {
		c.qmu.Lock()
		c.qbytes -= len(b)
		c.qmu.Unlock()
		_ = c.nc.SetWriteDeadline(time.Now().Add(writeTimeout))
		if _, err := c.nc.Write(b); err != nil {
			if !c.isClosed() {
				s.log("indi: client %s write failed (%v); dropping", c.id(), err)
			}
			s.removeConn(c)
			return
		}
	}
}

// send drops the connection when its queue is full: one stalled client must not
// stall the hub.
func (s *Server) send(c *conn, msg []byte) {
	if ok, full := c.enqueue(msg); !ok && full {
		s.log("indi: client %s outbound queue full; dropping", c.id())
		s.removeConn(c)
	}
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

func (s *Server) snapshotConns() []*conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	conns := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	return conns
}

// maxInbound caps one inbound top-level element; a var so tests can shrink it.
var maxInbound = 1 << 20

var errInboundTooLarge = errors.New("indi: inbound element exceeds size limit")

// inboundGuard caps bytes read since the last reset: xml.Decoder has no limit of
// its own, so an endless attribute or chardata run would buffer without bound.
type inboundGuard struct {
	r   io.Reader
	n   int64
	max int64
}

func (g *inboundGuard) Read(p []byte) (int, error) {
	if g.n > g.max {
		return 0, errInboundTooLarge
	}
	n, err := g.r.Read(p)
	g.n += int64(n)
	return n, err
}

func (g *inboundGuard) reset() { g.n = 0 }

// handle runs one connection's read loop; INDI is a stream of top-level elements,
// so it uses a Token loop rather than Decode, which expects a single root.
func (s *Server) handle(ctx context.Context, c *conn) {
	defer s.removeConn(c)
	// Closing the socket unblocks the read loop. The c.done arm matters: a bare
	// ctx watcher would leak a goroutine per disconnected client.
	go func() {
		select {
		case <-ctx.Done():
			_ = c.nc.Close()
		case <-c.done:
		}
	}()

	guard := &inboundGuard{r: c.nc, max: int64(maxInbound)}
	dec := xml.NewDecoder(guard)
	// C clients often open with encoding="ISO-8859-1", and encoding/xml kills the
	// stream on any non-UTF-8 declaration unless a CharsetReader is set.
	dec.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		switch strings.ToLower(charset) {
		case "us-ascii", "ascii", "iso-8859-1", "latin1", "utf-8":
			return input, nil
		}
		return nil, fmt.Errorf("indi: unsupported charset %q", charset)
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, errInboundTooLarge) {
				s.log("indi: client %s dropped: %v", c.id(), err)
			}
			return
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "getProperties":
			var v xgetProperties
			if dec.DecodeElement(&v, &se) == nil {
				s.dlog("indi: <- %s getProperties device=%q name=%q", c.id(), v.Device, v.Name)
				s.snapshot(c, v.Device, v.Name)
			}
		case "newNumberVector", "newSwitchVector", "newTextVector":
			var v xnewVector
			if dec.DecodeElement(&v, &se) == nil {
				s.dlog("indi: <- %s %s %s/%s [%s]", c.id(), se.Name.Local, v.Device, v.Name, newMembersStr(v))
				s.dispatchNew(c, se.Name.Local, v)
			}
		case "enableBLOB":
			var v xenableBLOB
			if dec.DecodeElement(&v, &se) == nil {
				mode := strings.TrimSpace(v.Value)
				if pol, ok := parseBlobPolicy(mode); ok {
					c.setPolicy(v.Device, v.Name, pol)
					s.dlog("indi: <- %s enableBLOB device=%q name=%q -> %s", c.id(), v.Device, v.Name, mode)
				} else {
					// Spec: an unrecognized mode leaves the policy unchanged.
					s.dlog("indi: <- %s enableBLOB device=%q name=%q: unrecognized mode %q ignored", c.id(), v.Device, v.Name, mode)
				}
			}
		default:
			// Includes newBLOBVector: client-to-device BLOB upload is unsupported.
			_ = dec.Skip()
		}
		guard.reset()
	}
}

// snapshot sends def*Vectors for the requested device/name to one client; empty
// means all.
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
			b, err := marshalDef(p, ts)
			if err != nil {
				s.log("indi: marshal def %s/%s failed: %v", p.Device, p.Name, err)
				continue
			}
			kind := kindOther
			if p.Type == BLOBType {
				kind = kindBLOBDef
			}
			if c.wants(p.Device, p.Name, kind) {
				s.send(c, frame(b))
			}
		}
	}
}

func newVecType(elem string) PropType {
	switch elem {
	case "newNumberVector":
		return NumberType
	case "newSwitchVector":
		return SwitchType
	default:
		return TextType
	}
}

// dispatchNew validates an inbound new*Vector before it reaches the device.
func (s *Server) dispatchNew(c *conn, elem string, v xnewVector) {
	d := s.device(v.Device)
	if d == nil {
		return
	}
	var p *Property
	for _, pp := range d.Properties() {
		if pp.Name == v.Name {
			p = pp
			break
		}
	}
	drop := func(why string) {
		s.dlog("indi: <- %s %s %s/%s dropped: %s", c.id(), elem, v.Device, v.Name, why)
		s.messageTo(c, v.Device, fmt.Sprintf("%s %q ignored: %s", elem, v.Name, why))
	}
	switch {
	case p == nil:
		drop("no such property")
		return
	case p.Perm == RO:
		// Spec: new*Vectors on read-only properties must be ignored.
		drop("property is read-only")
		return
	case p.Type != newVecType(elem):
		drop(fmt.Sprintf("property is a %v vector", p.Type))
		return
	}
	if p.Type == SwitchType && p.Rule != AnyOfMany {
		on := 0
		for _, sw := range v.Switches {
			if strings.EqualFold(strings.TrimSpace(sw.Value), "On") {
				on++
			}
		}
		if on > 1 {
			drop(fmt.Sprintf("rule %s allows at most one On member", p.Rule))
			return
		}
	}
	if p.Type == NumberType {
		_, members := p.snapshot()
		limits := make(map[string]Member, len(members))
		for _, m := range members {
			limits[m.Name] = m
		}
		for _, n := range v.Numbers {
			val, ok := ParseNumber(n.Value)
			if !ok {
				s.rejectNumber(p, fmt.Sprintf("%s: not a number: %q", n.Name, strings.TrimSpace(n.Value)))
				return
			}
			// Min==Max means unconstrained, the INDI convention.
			if m, exists := limits[n.Name]; exists && m.Min < m.Max && (val < m.Min || val > m.Max) {
				s.rejectNumber(p, fmt.Sprintf("%s: %g out of range [%g, %g]", n.Name, val, m.Min, m.Max))
				return
			}
		}
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

// rejectNumber reports an invalid newNumberVector the INDI way: Alert state plus
// a message.
func (s *Server) rejectNumber(p *Property, why string) {
	p.SetState(Alert)
	s.Update(p)
	s.Message(p.Device, p.Name+": "+why)
}

func (s *Server) messageTo(c *conn, device, msg string) {
	if b, err := xml.Marshal(xmessage{Device: device, Timestamp: now(), Message: msg}); err == nil {
		s.send(c, frame(b))
	}
}

func (s *Server) broadcast(b []byte, device, name string, kind msgKind) {
	msg := frame(b)
	for _, c := range s.snapshotConns() {
		if !c.wants(device, name, kind) {
			continue
		}
		s.send(c, msg)
	}
}

// Define broadcasts p's def*Vector to every client.
func (s *Server) Define(p *Property) {
	if b, err := marshalDef(p, now()); err == nil {
		kind := kindOther
		if p.Type == BLOBType {
			kind = kindBLOBDef
		}
		s.broadcast(b, p.Device, p.Name, kind)
	} else {
		s.log("indi: marshal def %s/%s failed: %v", p.Device, p.Name, err)
	}
}

// Update broadcasts p's current values and state as a set*Vector.
func (s *Server) Update(p *Property) {
	if b, err := marshalSet(p, now()); err == nil {
		s.broadcast(b, p.Device, p.Name, kindOther)
	} else {
		s.log("indi: marshal set %s/%s failed: %v", p.Device, p.Name, err)
	}
}

// Message broadcasts a free-text message attributed to device.
func (s *Server) Message(device, msg string) {
	if b, err := xml.Marshal(xmessage{Device: device, Timestamp: now(), Message: msg}); err == nil {
		s.broadcast(b, device, "", kindOther)
	} else {
		s.log("indi: marshal message for %s failed: %v", device, err)
	}
}

// Delete broadcasts a delProperty for device/name.
func (s *Server) Delete(device, name string) {
	if b, err := xml.Marshal(xdelProperty{Device: device, Name: name, Timestamp: now()}); err == nil {
		s.broadcast(b, device, name, kindOther)
	} else {
		s.log("indi: marshal delProperty %s/%s failed: %v", device, name, err)
	}
}

// SendBLOB delivers a one-element BLOB verbatim to every client whose enableBLOB
// policy admits it.
func (s *Server) SendBLOB(device, name, elem, format string, data []byte) {
	s.sendBLOB(device, name, elem, format, data, s.blobSize(format, data))
}

// SendBLOBSized is SendBLOB for a pre-compressed payload whose uncompressed size
// the caller already knows.
func (s *Server) SendBLOBSized(device, name, elem, format string, data []byte, uncompressedSize int) {
	s.sendBLOB(device, name, elem, format, data, uncompressedSize)
}

func (s *Server) sendBLOB(device, name, elem, format string, data []byte, size int) {
	msg := frame(blobSetXML(device, name, elem, format, data, size, now()))
	conns := s.snapshotConns()
	sent := 0
	for _, c := range conns {
		if !c.wants(device, name, kindBLOBSet) {
			continue
		}
		s.send(c, msg)
		sent++
	}
	s.dlog("indi: -> setBLOBVector %s/%s %d bytes (size attr %d) to %d/%d client(s)",
		device, name, len(data), size, sent, len(conns))
}

// blobSize computes the INDI size attr, which the DTD defines as the uncompressed
// byte count, so a ".z" format has to be inflated just to be counted.
func (s *Server) blobSize(format string, data []byte) int {
	if !strings.HasSuffix(format, ".z") {
		return len(data)
	}
	zr, err := zlib.NewReader(bytes.NewReader(data))
	if err == nil {
		n, cerr := io.Copy(io.Discard, zr)
		_ = zr.Close()
		if cerr == nil {
			return int(n)
		}
		err = cerr
	}
	s.log("indi: BLOB %s payload does not inflate (%v); size attr falls back to compressed length", format, err)
	return len(data)
}

var _ BLOBSizedSender = (*Server)(nil)

// frame appends the newline INDI peers expect between messages.
func frame(b []byte) []byte { return append(b, '\n') }

func now() string { return time.Now().UTC().Format("2006-01-02T15:04:05") }

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
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type gateDev struct {
	name  string
	mu    sync.Mutex
	calls map[string]int
	props []*Property
}

func newGateDev(name string) *gateDev {
	num := NewProperty(name, "LIMITED", NumberType, RW,
		&Member{Name: "LIM", Min: 0, Max: 10},
		&Member{Name: "FREE", Min: 0, Max: 0}) // Min==Max: no limit
	ro := NewProperty(name, "STATUS", NumberType, RO, &Member{Name: "VAL"})
	sel := NewProperty(name, "SEL", SwitchType, RW, &Member{Name: "A", On: true}, &Member{Name: "B"})
	sel.Rule = OneOfMany
	txt := NewProperty(name, "NOTE", TextType, RW, &Member{Name: "T"})
	blob := NewProperty(name, "CCD1", BLOBType, RO, &Member{Name: "CCD1"})
	props := []*Property{num, ro, sel, txt, blob}
	for _, p := range props {
		p.SetState(Ok)
	}
	return &gateDev{name: name, calls: map[string]int{}, props: props}
}

func (d *gateDev) Name() string            { return d.name }
func (d *gateDev) Properties() []*Property { return d.props }
func (d *gateDev) HandleNew(_ Publisher, name string, _ []NewMember) {
	d.mu.Lock()
	d.calls[name]++
	d.mu.Unlock()
}
func (d *gateDev) count(name string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls[name]
}
func (d *gateDev) prop(name string) *Property {
	for _, p := range d.props {
		if p.Name == name {
			return p
		}
	}
	return nil
}

func startHub(t *testing.T, devs ...Device) *Server {
	t.Helper()
	s := New("127.0.0.1:0")
	for _, d := range devs {
		if err := s.AddDevice(d); err != nil {
			t.Fatalf("AddDevice: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.Serve(ctx) }()
	for i := 0; i < 500; i++ {
		if s.Addr() != nil {
			return s
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("server did not start")
	return nil
}

func dialHub(t *testing.T, s *Server) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", s.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func readFor(c net.Conn, d time.Duration) string {
	_ = c.SetReadDeadline(time.Now().Add(d))
	var buf []byte
	tmp := make([]byte, 8192)
	for {
		n, err := c.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return string(buf)
		}
	}
}

func waitCond(t *testing.T, cond func() bool, what string) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (s *Server) connCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// TestStalledClientDroppedOthersLive checks that a client that stops reading is
// dropped once its queue fills, while the other clients keep working.
func TestStalledClientDroppedOthersLive(t *testing.T) {
	oldBytes, oldMsgs := outQueueBytes, outQueueMsgs
	outQueueBytes, outQueueMsgs = 256<<10, 16
	defer func() { outQueueBytes, outQueueMsgs = oldBytes, oldMsgs }()

	d := newGateDev("A")
	s := startHub(t, d)

	a := dialHub(t, s)
	var amu sync.Mutex
	var abuf bytes.Buffer
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := a.Read(buf)
			amu.Lock()
			abuf.Write(buf[:n])
			amu.Unlock()
			if err != nil {
				return
			}
		}
	}()

	b := dialHub(t, s)
	fmt.Fprint(b, `<enableBLOB device="A">Also</enableBLOB>`)
	waitCond(t, func() bool { return s.connCount() == 2 }, "both clients registered")
	time.Sleep(50 * time.Millisecond) // the policy must be registered before the sends below

	payload := make([]byte, 64<<10)
	num := d.prop("LIMITED")
	dropped := false
	for i := 0; i < 4000; i++ {
		s.SendBLOB("A", "CCD1", "CCD1", ".fits", payload)
		s.Update(num)
		if s.connCount() == 1 {
			dropped = true
			break
		}
	}
	if !dropped {
		t.Fatal("stalled client was never dropped")
	}

	fmt.Fprint(a, `<newSwitchVector device="A" name="SEL"><oneSwitch name="B">On</oneSwitch></newSwitchVector>`)
	waitCond(t, func() bool { return d.count("SEL") == 1 }, "command from the healthy client")
	amu.Lock()
	got := abuf.String()
	amu.Unlock()
	if !strings.Contains(got, "setNumberVector") {
		t.Error("healthy client stopped receiving updates while the stalled client was queued")
	}
	if strings.Contains(got, "setBLOBVector") {
		t.Error("client that never enabled BLOBs received one")
	}
}

// TestConnGoroutinesReturnToBaseline checks that repeated connect/disconnect
// cycles leak none of the per-connection goroutines.
func TestConnGoroutinesReturnToBaseline(t *testing.T) {
	s := startHub(t, newGateDev("A"))
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()
	for i := 0; i < 100; i++ {
		c, err := net.Dial("tcp", s.Addr().String())
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		fmt.Fprint(c, `<getProperties version="1.7"/>`)
		c.Close()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if n := runtime.NumGoroutine(); n <= before+10 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines: %d after 100 connect/disconnects, baseline %d", runtime.NumGoroutine(), before)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestEnableBLOBDeviceScoping checks that enabling BLOBs for one device does not
// enable them for another.
func TestEnableBLOBDeviceScoping(t *testing.T) {
	s := startHub(t, newGateDev("A"), newGateDev("B"))
	c := dialHub(t, s)
	fmt.Fprint(c, `<enableBLOB device="A">Also</enableBLOB>`)
	time.Sleep(50 * time.Millisecond)

	s.SendBLOB("A", "CCD1", "CCD1", ".fits", []byte{1})
	s.SendBLOB("B", "CCD1", "CCD1", ".fits", []byte{2})
	got := readFor(c, 300*time.Millisecond)
	if !strings.Contains(got, `<setBLOBVector device="A"`) {
		t.Error("BLOB for the enabled device was not delivered")
	}
	if strings.Contains(got, `<setBLOBVector device="B"`) {
		t.Error("BLOB for a device that was never enabled was delivered")
	}
}

// TestEnableBLOBNeverIsScoped checks that Never for one device leaves another
// device's BLOBs flowing.
func TestEnableBLOBNeverIsScoped(t *testing.T) {
	s := startHub(t, newGateDev("A"), newGateDev("B"))
	c := dialHub(t, s)
	fmt.Fprint(c, `<enableBLOB device="B">Also</enableBLOB><enableBLOB device="A">Never</enableBLOB>`)
	time.Sleep(50 * time.Millisecond)

	s.SendBLOB("A", "CCD1", "CCD1", ".fits", []byte{1})
	s.SendBLOB("B", "CCD1", "CCD1", ".fits", []byte{2})
	got := readFor(c, 300*time.Millisecond)
	if strings.Contains(got, `<setBLOBVector device="A"`) {
		t.Error("Never for device A did not suppress its BLOBs")
	}
	if !strings.Contains(got, `<setBLOBVector device="B"`) {
		t.Error("Never for device A also killed device B's BLOBs")
	}
}

// TestEnableBLOBOnlySuppressesChatter checks that Only delivers BLOB traffic and
// nothing else.
func TestEnableBLOBOnlySuppressesChatter(t *testing.T) {
	d := newGateDev("A")
	s := startHub(t, d)
	c := dialHub(t, s)
	fmt.Fprint(c, `<enableBLOB device="A">Only</enableBLOB>`)
	time.Sleep(50 * time.Millisecond)

	s.Update(d.prop("LIMITED"))
	s.Define(d.prop("CCD1"))
	s.SendBLOB("A", "CCD1", "CCD1", ".fits", []byte{1, 2, 3})
	got := readFor(c, 300*time.Millisecond)
	if strings.Contains(got, "setNumberVector") {
		t.Error("Only connection received non-BLOB chatter")
	}
	if !strings.Contains(got, "defBLOBVector") {
		t.Error("Only connection did not receive defBLOBVector metadata")
	}
	if !strings.Contains(got, "setBLOBVector") {
		t.Error("Only connection did not receive the BLOB payload")
	}
}

// TestEnableBLOBPropertyNarrowingWins checks that a property-level policy
// overrides the device-level one in both directions.
func TestEnableBLOBPropertyNarrowingWins(t *testing.T) {
	s := startHub(t, newGateDev("A"))

	c := dialHub(t, s)
	fmt.Fprint(c, `<enableBLOB device="A">Never</enableBLOB><enableBLOB device="A" name="CCD1">Also</enableBLOB>`)
	time.Sleep(50 * time.Millisecond)
	s.SendBLOB("A", "CCD1", "CCD1", ".fits", []byte{1})
	if got := readFor(c, 300*time.Millisecond); !strings.Contains(got, "setBLOBVector") {
		t.Error("property-level Also did not override device-level Never")
	}

	c2 := dialHub(t, s)
	fmt.Fprint(c2, `<enableBLOB device="A">Also</enableBLOB><enableBLOB device="A" name="CCD1">Never</enableBLOB>`)
	time.Sleep(50 * time.Millisecond)
	s.SendBLOB("A", "CCD1", "CCD1", ".fits", []byte{1})
	if got := readFor(c2, 300*time.Millisecond); strings.Contains(got, "setBLOBVector") {
		t.Error("property-level Never did not override device-level Also")
	}
}

// TestEnableBLOBUnrecognizedModeIgnored checks that a garbage mode leaves the
// policy at its Never default.
func TestEnableBLOBUnrecognizedModeIgnored(t *testing.T) {
	s := startHub(t, newGateDev("A"))
	c := dialHub(t, s)
	fmt.Fprint(c, `<enableBLOB device="A">Sometimes</enableBLOB>`)
	time.Sleep(50 * time.Millisecond)
	s.SendBLOB("A", "CCD1", "CCD1", ".fits", []byte{1})
	if got := readFor(c, 300*time.Millisecond); strings.Contains(got, "setBLOBVector") {
		t.Error("unrecognized enableBLOB mode enabled delivery (default is Never)")
	}
}

// TestLightVectorMarshal checks the def and set forms of a Light vector.
func TestLightVectorMarshal(t *testing.T) {
	p := NewProperty("Weather", "WEATHER_STATUS", LightType, RO,
		&Member{Name: "TEMP", Label: "Temperature", Light: Ok},
		&Member{Name: "WIND", Light: Alert})
	p.Label, p.Group = "Status", "Main"
	p.SetState(Busy)

	def, err := marshalDef(p, "2026-01-01T00:00:00")
	if err != nil {
		t.Fatalf("marshalDef(Light): %v", err)
	}
	// The DTD gives defLightVector neither a perm nor a rule attribute.
	if strings.Contains(string(def), "perm=") || strings.Contains(string(def), "rule=") {
		t.Errorf("defLightVector must carry no perm/rule attrs:\n%s", def)
	}
	var dv struct {
		XMLName xml.Name
		State   string `xml:"state,attr"`
		Lights  []struct {
			Name  string `xml:"name,attr"`
			Value string `xml:",chardata"`
		} `xml:"defLight"`
	}
	if err := xml.Unmarshal(def, &dv); err != nil {
		t.Fatalf("def does not parse: %v\n%s", err, def)
	}
	if dv.XMLName.Local != "defLightVector" || dv.State != "Busy" || len(dv.Lights) != 2 {
		t.Fatalf("def = %+v\n%s", dv, def)
	}
	valid := map[string]bool{"Idle": true, "Ok": true, "Busy": true, "Alert": true}
	if !valid[dv.Lights[0].Value] || !valid[dv.Lights[1].Value] {
		t.Errorf("defLight chardata must be a state token: %+v", dv.Lights)
	}
	if dv.Lights[0].Value != "Ok" || dv.Lights[1].Value != "Alert" {
		t.Errorf("member states: got %q/%q want Ok/Alert", dv.Lights[0].Value, dv.Lights[1].Value)
	}

	set, err := marshalSet(p, "2026-01-01T00:00:01")
	if err != nil {
		t.Fatalf("marshalSet(Light): %v", err)
	}
	var sv struct {
		XMLName xml.Name
		Lights  []struct {
			Name  string `xml:"name,attr"`
			Value string `xml:",chardata"`
		} `xml:"oneLight"`
	}
	if err := xml.Unmarshal(set, &sv); err != nil {
		t.Fatalf("set does not parse: %v\n%s", err, set)
	}
	if sv.XMLName.Local != "setLightVector" || len(sv.Lights) != 2 || sv.Lights[1].Value != "Alert" {
		t.Fatalf("set = %+v\n%s", sv, set)
	}
}

// TestLightVectorReachesWire checks that a Light property is enumerated to a
// client.
func TestLightVectorReachesWire(t *testing.T) {
	d := newGateDev("A")
	light := NewProperty("A", "WEATHER_STATUS", LightType, RO, &Member{Name: "SAFE", Light: Ok})
	light.SetState(Ok)
	d.props = append(d.props, light)
	s := startHub(t, d)
	c := dialHub(t, s)
	fmt.Fprint(c, `<getProperties version="1.7"/>`)
	got := readFor(c, 300*time.Millisecond)
	if !strings.Contains(got, "<defLightVector") || !strings.Contains(got, ">Ok</defLight>") {
		t.Errorf("Light property missing from enumeration:\n%s", got)
	}
}

// TestDispatchNewGate checks which inbound new*Vectors reach HandleNew and which
// are rejected.
func TestDispatchNewGate(t *testing.T) {
	d := newGateDev("Gate")
	s := startHub(t, d)
	c := dialHub(t, s)

	send := func(x string) {
		t.Helper()
		if _, err := io.WriteString(c, x); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	// All of these must be dropped before HandleNew:
	send(`<newNumberVector device="Gate" name="NOPE"><oneNumber name="X">1</oneNumber></newNumberVector>`)                                   // no such property
	send(`<newNumberVector device="Gate" name="STATUS"><oneNumber name="VAL">1</oneNumber></newNumberVector>`)                               // read-only
	send(`<newTextVector device="Gate" name="LIMITED"><oneText name="LIM">hi</oneText></newTextVector>`)                                     // type mismatch
	send(`<newSwitchVector device="Gate" name="SEL"><oneSwitch name="A">On</oneSwitch><oneSwitch name="B">On</oneSwitch></newSwitchVector>`) // OneOfMany violation
	send(`<newNumberVector device="Gate" name="LIMITED"><oneNumber name="LIM">50</oneNumber></newNumberVector>`)                             // out of range
	send(`<newNumberVector device="Gate" name="LIMITED"><oneNumber name="LIM">bogus</oneNumber></newNumberVector>`)                          // unparseable
	// These must pass (FREE has Min==Max, i.e. no limit; sexagesimal parses):
	send(`<newNumberVector device="Gate" name="LIMITED"><oneNumber name="LIM">2:30:00</oneNumber><oneNumber name="FREE">1e9</oneNumber></newNumberVector>`)
	send(`<newSwitchVector device="Gate" name="SEL"><oneSwitch name="B">On</oneSwitch></newSwitchVector>`)

	waitCond(t, func() bool { return d.count("SEL") == 1 && d.count("LIMITED") == 1 }, "valid vectors to arrive")
	if n := d.count("NOPE") + d.count("STATUS") + d.count("NOTE"); n != 0 {
		t.Errorf("gated vectors reached HandleNew %d time(s)", n)
	}
	if d.count("LIMITED") != 1 {
		t.Errorf("LIMITED HandleNew count = %d, want 1 (only the in-range write)", d.count("LIMITED"))
	}
	if st := d.prop("LIMITED").State(); st != Alert {
		t.Errorf("LIMITED state after out-of-range write = %v, want Alert", st)
	}
	got := readFor(c, 300*time.Millisecond)
	if !strings.Contains(got, "out of range") {
		t.Errorf("no out-of-range message reached the client:\n%s", got)
	}
	if !strings.Contains(got, "ignored") {
		t.Errorf("no rejection message for dropped vectors reached the sender:\n%s", got)
	}
}

type fakeListener struct {
	steps chan any // errors and net.Conns served in order; closing it closes the listener
	addr  net.Addr
}

func (l *fakeListener) Accept() (net.Conn, error) {
	v, ok := <-l.steps
	if !ok {
		return nil, net.ErrClosed
	}
	if err, isErr := v.(error); isErr {
		return nil, err
	}
	return v.(net.Conn), nil
}
func (l *fakeListener) Close() error   { return nil }
func (l *fakeListener) Addr() net.Addr { return l.addr }

// TestAcceptLoopSurvivesTransientError checks that a transient Accept error does
// not end the loop but a closed listener does.
func TestAcceptLoopSurvivesTransientError(t *testing.T) {
	s := New("127.0.0.1:0")
	if err := s.AddDevice(newGateDev("A")); err != nil {
		t.Fatal(err)
	}
	srvSide, cliSide := net.Pipe()
	t.Cleanup(func() { srvSide.Close(); cliSide.Close() })
	l := &fakeListener{steps: make(chan any, 2), addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}}
	l.steps <- errors.New("accept tcp: too many open files")
	l.steps <- srvSide
	close(l.steps)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.acceptLoop(ctx, l) }()

	if _, err := fmt.Fprint(cliSide, `<getProperties version="1.7"/>`); err != nil {
		t.Fatalf("write after transient error: %v", err)
	}
	_ = cliSide.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 8192)
	n, err := cliSide.Read(buf)
	if err != nil {
		t.Fatalf("conn accepted after transient error was not served: %v", err)
	}
	if !strings.Contains(string(buf[:n]), "def") {
		t.Errorf("expected a def*Vector, got %q", buf[:n])
	}
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("acceptLoop returned %v, want a net.ErrClosed wrap", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("acceptLoop did not return after listener close")
	}
}

// TestOversizedInboundDropsClientOnly checks that an oversized element kills only
// that connection, leaving the hub serving.
func TestOversizedInboundDropsClientOnly(t *testing.T) {
	s := startHub(t, newGateDev("A"))
	bad := dialHub(t, s)

	if _, err := fmt.Fprint(bad, `<getProperties version="`); err != nil {
		t.Fatal(err)
	}
	junk := bytes.Repeat([]byte("a"), 64<<10)
	for wrote := 0; wrote < 3*maxInbound; {
		n, err := bad.Write(junk)
		wrote += n
		if err != nil {
			break
		}
	}
	// A read that drains to EOF or reset means the server hung up; a deadline
	// error would mean it is still buffering.
	_ = bad.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.Copy(io.Discard, bad); errors.Is(err, os.ErrDeadlineExceeded) {
		t.Error("server left the oversized client connected")
	}

	good := dialHub(t, s)
	fmt.Fprint(good, `<getProperties version="1.7"/>`)
	if got := readFor(good, 500*time.Millisecond); !strings.Contains(got, "defNumberVector") {
		t.Errorf("hub not serving new clients after oversized peer:\n%q", got)
	}
}

// TestParseNumber checks the decimal and sexagesimal spellings ParseNumber accepts.
func TestParseNumber(t *testing.T) {
	cases := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"3.14", 3.14, true},
		{"-12", -12, true},
		{"12:34:56.7", 12 + 34/60.0 + 56.7/3600.0, true},
		{"-5 30 00", -5.5, true},
		{"  1:30  ", 1.5, true},
		{"-0 30", -0.5, true},
		{"1e3", 1000, true},
		{"garbage", 0, false},
		{"", 0, false},
		{"1:xx", 0, false},
		{"1:2:3:4", 0, false},
		{"12ms", 0, false},
	}
	for _, tc := range cases {
		got, ok := ParseNumber(tc.in)
		if ok != tc.ok {
			t.Errorf("ParseNumber(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			continue
		}
		if ok && (got-tc.want > 1e-9 || tc.want-got > 1e-9) {
			t.Errorf("ParseNumber(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestBlobSizeSemantics checks that the BLOB size attr is the uncompressed byte
// count.
func TestBlobSizeSemantics(t *testing.T) {
	orig := bytes.Repeat([]byte("pixel data "), 1000)
	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	if _, err := zw.Write(orig); err != nil {
		t.Fatal(err)
	}
	zw.Close()

	s := New(":0")
	if got := s.blobSize(".fits.z", zbuf.Bytes()); got != len(orig) {
		t.Errorf("blobSize(.fits.z) = %d, want uncompressed %d", got, len(orig))
	}
	if got := s.blobSize(".fits", orig); got != len(orig) {
		t.Errorf("blobSize(.fits) = %d, want %d", got, len(orig))
	}
	if got := s.blobSize(".fits.z", []byte{1, 2, 3}); got != 3 {
		t.Errorf("blobSize(bad .z) = %d, want fallback 3", got)
	}

	raw := blobSetXML("A", "CCD1", "CCD1", ".fits.z", zbuf.Bytes(), len(orig), now())
	if !bytes.Contains(raw, []byte(fmt.Sprintf(`size="%d"`, len(orig)))) {
		t.Errorf("blobSetXML did not carry the uncompressed size %d", len(orig))
	}
}

// TestSendBLOBCompressedSizeOnWire checks that the SendBLOB path puts the
// uncompressed count on the wire for a pre-compressed payload.
func TestSendBLOBCompressedSizeOnWire(t *testing.T) {
	s := startHub(t, newGateDev("A"))
	c := dialHub(t, s)
	fmt.Fprint(c, `<enableBLOB device="A">Also</enableBLOB>`)
	time.Sleep(50 * time.Millisecond)

	orig := bytes.Repeat([]byte{0xAB}, 4096)
	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	zw.Write(orig)
	zw.Close()

	s.SendBLOB("A", "CCD1", "CCD1", ".fits.z", zbuf.Bytes())
	got := readFor(c, 500*time.Millisecond)
	if !strings.Contains(got, fmt.Sprintf(`size="%d"`, len(orig))) {
		t.Errorf("wire size attr is not the uncompressed count %d:\n%.300s", len(orig), got)
	}

	s.SendBLOBSized("A", "CCD1", "CCD1", ".fits.z", zbuf.Bytes(), 777777)
	got = readFor(c, 500*time.Millisecond)
	if !strings.Contains(got, `size="777777"`) {
		t.Errorf("SendBLOBSized size attr missing:\n%.300s", got)
	}
}

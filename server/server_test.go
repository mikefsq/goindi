package server_test

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mikefsq/goindi/server"
)

type blobDev struct{ props []*server.Property }

func newBlobDev() *blobDev {
	p := server.NewProperty("Blob", "CCD1", server.BLOBType, server.RO,
		&server.Member{Name: "CCD1", Label: "Image"})
	p.SetState(server.Ok)
	return &blobDev{props: []*server.Property{server.ConnectionProperty("Blob"), p}}
}

func (b *blobDev) Name() string                                           { return "Blob" }
func (b *blobDev) Properties() []*server.Property                         { return b.props }
func (b *blobDev) HandleNew(server.Publisher, string, []server.NewMember) {}

func drainFor(c net.Conn, d time.Duration) string {
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

// TestSendBLOBGating checks that a BLOB reaches only the clients that asked for
// it with enableBLOB.
func TestSendBLOBGating(t *testing.T) {
	s := startServer(t, newBlobDev())

	enabled, err := net.Dial("tcp", s.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { enabled.Close() })
	plain, err := net.Dial("tcp", s.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { plain.Close() })

	fmt.Fprint(enabled, `<getProperties version="1.7"/>`)
	fmt.Fprint(plain, `<getProperties version="1.7"/>`)
	drainFor(enabled, 100*time.Millisecond)
	drainFor(plain, 100*time.Millisecond)

	fmt.Fprint(enabled, `<enableBLOB device="Blob" name="CCD1">Also</enableBLOB>`)
	time.Sleep(80 * time.Millisecond) // the server must register the policy before the send below

	s.SendBLOB("Blob", "CCD1", "CCD1", ".fits", []byte{0, 1, 2, 3, 4})

	got := drainFor(enabled, 500*time.Millisecond)
	if !strings.Contains(got, "setBLOBVector") || !strings.Contains(got, `format=".fits"`) {
		t.Errorf("enabled client did not receive the BLOB; got %q", got)
	}
	if other := drainFor(plain, 200*time.Millisecond); strings.Contains(other, "setBLOBVector") {
		t.Errorf("client that did not enableBLOB received one; got %q", other)
	}
}

type fakeDev struct {
	mu    sync.Mutex
	got   map[string][]server.NewMember
	props []*server.Property
}

func newFakeDev() *fakeDev {
	return &fakeDev{
		got: map[string][]server.NewMember{},
		props: []*server.Property{
			server.ConnectionProperty("Fake"),
			server.EquatorialCoordProperty("Fake"),
		},
	}
}

func (f *fakeDev) Name() string                   { return "Fake" }
func (f *fakeDev) Properties() []*server.Property { return f.props }
func (f *fakeDev) HandleNew(pub server.Publisher, name string, m []server.NewMember) {
	f.mu.Lock()
	f.got[name] = m
	f.mu.Unlock()
	for _, p := range f.props {
		if p.Name == name {
			p.SetState(server.Ok)
			pub.Update(p)
		}
	}
}
func (f *fakeDev) received(name string) []server.NewMember {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.got[name]
}

type elem struct {
	XMLName   xml.Name
	Device    string `xml:"device,attr"`
	Name      string `xml:"name,attr"`
	State     string `xml:"state,attr"`
	DefNumber []struct {
		Name string `xml:"name,attr"`
	} `xml:"defNumber"`
	DefSwitch []struct {
		Name string `xml:"name,attr"`
	} `xml:"defSwitch"`
	OneSwitch []struct {
		Name  string `xml:"name,attr"`
		Value string `xml:",chardata"`
	} `xml:"oneSwitch"`
}

func startServer(t *testing.T, devs ...server.Device) *server.Server {
	t.Helper()
	s := server.New("127.0.0.1:0")
	for _, d := range devs {
		if err := s.AddDevice(d); err != nil {
			t.Fatalf("AddDevice: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.Serve(ctx) }()
	for i := 0; i < 200; i++ {
		if s.Addr() != nil {
			return s
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("server did not start")
	return nil
}

func dial(t *testing.T, s *server.Server) (net.Conn, *xml.Decoder) {
	t.Helper()
	c, err := net.Dial("tcp", s.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	return c, xml.NewDecoder(c)
}

func readElem(t *testing.T, dec *xml.Decoder) elem {
	t.Helper()
	for {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if se, ok := tok.(xml.StartElement); ok {
			var e elem
			if err := dec.DecodeElement(&e, &se); err != nil {
				t.Fatalf("decode: %v", err)
			}
			return e
		}
	}
}

// TestGetPropertiesEnumeratesDevice checks that getProperties returns a def for
// every property of a device.
func TestGetPropertiesEnumeratesDevice(t *testing.T) {
	s := startServer(t, newFakeDev())
	c, dec := dial(t, s)
	fmt.Fprint(c, `<getProperties version="1.7"/>`)

	byName := map[string]elem{}
	for i := 0; i < 2; i++ {
		e := readElem(t, dec)
		byName[e.Name] = e
	}
	if e, ok := byName["CONNECTION"]; !ok || e.XMLName.Local != "defSwitchVector" {
		t.Errorf("CONNECTION: got %+v", e)
	}
	if e, ok := byName["EQUATORIAL_EOD_COORD"]; !ok || e.XMLName.Local != "defNumberVector" || len(e.DefNumber) != 2 {
		t.Errorf("EQUATORIAL_EOD_COORD: got %+v", e)
	}
}

// TestNewVectorDispatchesAndEchoes checks that a new*Vector reaches the device
// and the resulting set*Vector reaches the client.
func TestNewVectorDispatchesAndEchoes(t *testing.T) {
	f := newFakeDev()
	s := startServer(t, f)
	c, dec := dial(t, s)

	fmt.Fprint(c, `<getProperties version="1.7"/>`)
	readElem(t, dec)
	readElem(t, dec)

	fmt.Fprint(c, `<newSwitchVector device="Fake" name="CONNECTION"><oneSwitch name="CONNECT">On</oneSwitch></newSwitchVector>`)

	e := readElem(t, dec)
	if e.XMLName.Local != "setSwitchVector" || e.Name != "CONNECTION" || e.State != "Ok" {
		t.Fatalf("echo: got %+v", e)
	}
	got := f.received("CONNECTION")
	if len(got) != 1 || got[0].Name != "CONNECT" || !got[0].On() {
		t.Errorf("device received %+v", got)
	}
}

// TestCharsetProcInst checks that an ISO-8859-1 XML declaration does not kill the
// connection.
func TestCharsetProcInst(t *testing.T) {
	s := startServer(t, newFakeDev())
	c, dec := dial(t, s)
	fmt.Fprint(c, `<?xml version="1.0" encoding="ISO-8859-1"?><getProperties version="1.7"/>`)
	if e := readElem(t, dec); e.Device != "Fake" {
		t.Errorf("expected a def for device Fake after latin-1 ProcInst, got %+v", e)
	}
}

// TestMalformedXMLDropsClientOnly checks that garbage bytes drop only the
// offending client, leaving the hub serving.
func TestMalformedXMLDropsClientOnly(t *testing.T) {
	s := startServer(t, newFakeDev())

	bad, decBad := dial(t, s)
	fmt.Fprint(bad, `<getProperties version="1.7"/>`)
	readElem(t, decBad)
	readElem(t, decBad)

	fmt.Fprint(bad, "<<<\x01 not xml %%%")
	// A clean EOF means the server closed the connection; a deadline error would
	// mean it left the client connected.
	if _, err := io.ReadAll(bad); err != nil {
		t.Errorf("server did not drop the malformed client: read err %v", err)
	}

	good, decGood := dial(t, s)
	fmt.Fprint(good, `<getProperties version="1.7"/>`)
	if e := readElem(t, decGood); e.Device != "Fake" {
		t.Errorf("second client not served after malformed peer, got %+v", e)
	}
}

type starterDev struct {
	name    string
	started chan struct{}
	props   []*server.Property
}

func newStarterDev(name string) *starterDev {
	return &starterDev{name: name, started: make(chan struct{}),
		props: []*server.Property{server.ConnectionProperty(name)}}
}

func (d *starterDev) Name() string                                           { return d.name }
func (d *starterDev) Properties() []*server.Property                         { return d.props }
func (d *starterDev) HandleNew(server.Publisher, string, []server.NewMember) {}
func (d *starterDev) Start(ctx context.Context, pub server.Publisher)        { close(d.started) }

// TestAddDeviceAfterServeStartsStarter checks that a Starter registered while the
// hub is already serving is started and enumerated.
func TestAddDeviceAfterServeStartsStarter(t *testing.T) {
	s := startServer(t)
	late := newStarterDev("Late")
	if err := s.AddDevice(late); err != nil {
		t.Fatalf("AddDevice after Serve: %v", err)
	}
	select {
	case <-late.started:
	case <-time.After(2 * time.Second):
		t.Fatal("Starter added after Serve was never started")
	}
	c, dec := dial(t, s)
	fmt.Fprint(c, `<getProperties version="1.7"/>`)
	if e := readElem(t, dec); e.Device != "Late" {
		t.Errorf("late device not enumerated: %+v", e)
	}
}

// TestSwitchRules checks that SetSwitch enforces each SwitchRule.
func TestSwitchRules(t *testing.T) {
	one := server.ConnectionProperty("Dev")
	one.SetSwitch("DISCONNECT", false)
	if !one.Switch("DISCONNECT") {
		t.Error("OneOfMany: Off leaving zero members On must be refused")
	}
	one.SetSwitch("CONNECT", true)
	if !one.Switch("CONNECT") || one.Switch("DISCONNECT") {
		t.Error("OneOfMany: turning CONNECT On must turn DISCONNECT Off")
	}

	pier := server.PierSideProperty("Dev")
	pier.SetSwitch("PIER_WEST", true)
	pier.SetSwitch("PIER_EAST", true)
	if pier.Switch("PIER_WEST") || !pier.Switch("PIER_EAST") {
		t.Error("AtMostOne: turning one On must turn the others Off")
	}
	pier.SetSwitch("PIER_EAST", false)
	if pier.Switch("PIER_EAST") || pier.Switch("PIER_WEST") {
		t.Error("AtMostOne: all-Off must be allowed")
	}

	any := server.NewProperty("Dev", "FLAGS", server.SwitchType, server.RW,
		&server.Member{Name: "A", On: true}, &server.Member{Name: "B"})
	any.Rule = server.AnyOfMany
	any.SetSwitch("B", true)
	if !any.Switch("A") || !any.Switch("B") {
		t.Error("AnyOfMany: members must toggle independently")
	}
	any.SetSwitch("A", false)
	any.SetSwitch("B", false)
	if any.Switch("A") || any.Switch("B") {
		t.Error("AnyOfMany: all-Off must be allowed")
	}
}

// TestDuplicateDeviceRejected checks that AddDevice refuses a name already in use.
func TestDuplicateDeviceRejected(t *testing.T) {
	s := server.New("127.0.0.1:0")
	if err := s.AddDevice(newFakeDev()); err != nil {
		t.Fatalf("first AddDevice: %v", err)
	}
	if err := s.AddDevice(newFakeDev()); err == nil {
		t.Error("duplicate device name should be rejected")
	}
}

package server_test

import (
	"context"
	"encoding/xml"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mikefsq/goindi/server"
)

// blobDev exposes a single CCD1 BLOB property, for the BLOB-gating test.
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

// TestSendBLOBGating verifies a BLOB reaches only clients that asked for it via
// enableBLOB — the INDI rule PHD2 relies on (it sends enableBLOB before capturing).
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
	drainFor(enabled, 100*time.Millisecond) // consume the initial defs
	drainFor(plain, 100*time.Millisecond)

	fmt.Fprint(enabled, `<enableBLOB device="Blob" name="CCD1">Also</enableBLOB>`)
	time.Sleep(80 * time.Millisecond) // let the server register it

	s.SendBLOB("Blob", "CCD1", "CCD1", ".fits", []byte{0, 1, 2, 3, 4})

	got := drainFor(enabled, 500*time.Millisecond)
	if !strings.Contains(got, "setBLOBVector") || !strings.Contains(got, `format=".fits"`) {
		t.Errorf("enabled client did not receive the BLOB; got %q", got)
	}
	if other := drainFor(plain, 200*time.Millisecond); strings.Contains(other, "setBLOBVector") {
		t.Errorf("client that did not enableBLOB received one; got %q", other)
	}
}

// fakeDev is a minimal Device: it records the new-vectors it receives and echoes
// the touched property back as a set-vector.
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

// elem is a permissive decode target for any INDI element the server sends.
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

func TestNewVectorDispatchesAndEchoes(t *testing.T) {
	f := newFakeDev()
	s := startServer(t, f)
	c, dec := dial(t, s)

	fmt.Fprint(c, `<getProperties version="1.7"/>`)
	readElem(t, dec)
	readElem(t, dec) // drain the two defs

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

func TestDuplicateDeviceRejected(t *testing.T) {
	s := server.New("127.0.0.1:0")
	if err := s.AddDevice(newFakeDev()); err != nil {
		t.Fatalf("first AddDevice: %v", err)
	}
	if err := s.AddDevice(newFakeDev()); err == nil {
		t.Error("duplicate device name should be rejected")
	}
}

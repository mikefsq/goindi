package mount_test

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/mikefsq/goindi/mount"
	"github.com/mikefsq/goindi/server"
	"github.com/mikefsq/lx200"
)

// fakeMount is a lx200.Mount (+ Guider, OpLocker, PierSider) recording what the
// INDI device drives it to do.
type fakeMount struct {
	mu     sync.Mutex
	opMu   sync.Mutex
	curRA  float64
	curDec float64
	tRA    float64
	tDec   float64
	slewed bool
	synced bool
	halted bool
	pulses []pulse
	side   lx200.PierSide
}

type pulse struct {
	Dir lx200.Direction
	Ms  int
}

func (f *fakeMount) RA() (float64, error)  { f.mu.Lock(); defer f.mu.Unlock(); return f.curRA, nil }
func (f *fakeMount) Dec() (float64, error) { f.mu.Lock(); defer f.mu.Unlock(); return f.curDec, nil }
func (f *fakeMount) SetTargetRA(h float64) (bool, error) {
	f.mu.Lock()
	f.tRA = h
	f.mu.Unlock()
	return true, nil
}
func (f *fakeMount) SetTargetDec(d float64) (bool, error) {
	f.mu.Lock()
	f.tDec = d
	f.mu.Unlock()
	return true, nil
}
func (f *fakeMount) SlewToTarget() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.curRA, f.curDec, f.slewed = f.tRA, f.tDec, true
	return nil
}
func (f *fakeMount) SyncToTarget() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.curRA, f.curDec, f.synced = f.tRA, f.tDec, true
	return "Matched", nil
}
func (f *fakeMount) Halt() error             { f.mu.Lock(); f.halted = true; f.mu.Unlock(); return nil }
func (f *fakeMount) Slewing() (bool, error)  { return false, nil }
func (f *fakeMount) Tracking() (bool, error) { return true, nil }
func (f *fakeMount) SetTracking(bool) error  { return nil }
func (f *fakeMount) PulseGuide(d lx200.Direction, ms int) error {
	f.mu.Lock()
	f.pulses = append(f.pulses, pulse{d, ms})
	f.mu.Unlock()
	return nil
}
func (f *fakeMount) PierSide() (lx200.PierSide, error) { return f.side, nil }
func (f *fakeMount) OpLock() func()                    { f.opMu.Lock(); return f.opMu.Unlock }

// mountState is a mutex-free snapshot of the recorded state.
type mountState struct {
	tRA, tDec      float64
	slewed, synced bool
	halted         bool
	pulses         []pulse
}

func (f *fakeMount) snap() mountState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return mountState{
		tRA: f.tRA, tDec: f.tDec, slewed: f.slewed, synced: f.synced, halted: f.halted,
		pulses: append([]pulse(nil), f.pulses...),
	}
}

var (
	_ lx200.Mount     = (*fakeMount)(nil)
	_ lx200.Guider    = (*fakeMount)(nil)
	_ lx200.OpLocker  = (*fakeMount)(nil)
	_ lx200.PierSider = (*fakeMount)(nil)
)

// capPub captures published updates/messages.
type capPub struct {
	mu      sync.Mutex
	updated []string
	msgs    []string
}

func (c *capPub) Define(p *server.Property)            { c.note(p.Name) }
func (c *capPub) Update(p *server.Property)            { c.note(p.Name) }
func (c *capPub) Message(_, m string)                  { c.mu.Lock(); c.msgs = append(c.msgs, m); c.mu.Unlock() }
func (c *capPub) Delete(_, _ string)                   {}
func (c *capPub) SendBLOB(_, _, _, _ string, _ []byte) {}
func (c *capPub) note(n string)                        { c.mu.Lock(); c.updated = append(c.updated, n); c.mu.Unlock() }

func nm(name, val string) server.NewMember { return server.NewMember{Name: name, Value: val} }

func newDev() (*mount.Device, *fakeMount) {
	f := &fakeMount{}
	return mount.New("TestScope", func() (lx200.Mount, error) { return f, nil }), f
}

func TestConnect(t *testing.T) {
	d, _ := newDev()
	pub := &capPub{}
	d.HandleNew(pub, "CONNECTION", []server.NewMember{nm("CONNECT", "On")})
	// The connection property should have been published as Ok with CONNECT on.
	for _, p := range d.Properties() {
		if p.Name == "CONNECTION" {
			if p.State() != server.Ok || !p.Switch("CONNECT") {
				t.Errorf("CONNECTION state=%v connect=%v", p.State(), p.Switch("CONNECT"))
			}
		}
	}
}

func TestSlew(t *testing.T) {
	d, f := newDev()
	pub := &capPub{}
	d.HandleNew(pub, "ON_COORD_SET", []server.NewMember{nm("SLEW", "On")})
	d.HandleNew(pub, "EQUATORIAL_EOD_COORD", []server.NewMember{nm("RA", "5.5"), nm("DEC", "22.0")})

	waitFor(t, func() bool { s := f.snap(); return s.slewed }, "slew")
	s := f.snap()
	if !approx(s.tRA, 5.5) || !approx(s.tDec, 22.0) || s.synced {
		t.Errorf("after slew: tRA=%v tDec=%v synced=%v", s.tRA, s.tDec, s.synced)
	}
}

func TestSync(t *testing.T) {
	d, f := newDev()
	pub := &capPub{}
	d.HandleNew(pub, "ON_COORD_SET", []server.NewMember{nm("SYNC", "On")})
	d.HandleNew(pub, "EQUATORIAL_EOD_COORD", []server.NewMember{nm("RA", "12.0"), nm("DEC", "-5.0")})

	waitFor(t, func() bool { s := f.snap(); return s.synced }, "sync")
	if s := f.snap(); s.slewed {
		t.Error("sync should not slew")
	}
}

func TestPulseGuide(t *testing.T) {
	d, f := newDev()
	pub := &capPub{}
	d.HandleNew(pub, "TELESCOPE_TIMED_GUIDE_NS", []server.NewMember{nm("TIMED_GUIDE_N", "512")})
	s := f.snap()
	if len(s.pulses) != 1 || s.pulses[0].Dir != lx200.North || s.pulses[0].Ms != 512 {
		t.Errorf("pulses = %+v", s.pulses)
	}
}

// fakeOptics is a mutable Optics holder (millimetres).
type fakeOptics struct {
	mu               sync.Mutex
	ap, fl, gap, gfl float64
}

func (o *fakeOptics) OpticsMM() (float64, float64, float64, float64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.ap, o.fl, o.gap, o.gfl
}

func TestTelescopeInfoReportsOptics(t *testing.T) {
	opt := &fakeOptics{ap: 200, fl: 1600, gap: 60, gfl: 240}
	d := mount.New("TestScope", func() (lx200.Mount, error) { return &fakeMount{}, nil }, mount.WithOptics(opt))

	var info *server.Property
	for _, p := range d.Properties() {
		if p.Name == "TELESCOPE_INFO" {
			info = p
		}
	}
	if info == nil {
		t.Fatal("TELESCOPE_INFO not exposed when optics provided")
	}
	if info.Perm != server.RO || info.Type != server.NumberType {
		t.Errorf("TELESCOPE_INFO type=%v perm=%v", info.Type, info.Perm)
	}
	if got := info.Number("TELESCOPE_FOCAL_LENGTH"); !approx(got, 1600) {
		t.Errorf("TELESCOPE_FOCAL_LENGTH = %v want 1600", got)
	}
	if got := info.Number("GUIDER_APERTURE"); !approx(got, 60) {
		t.Errorf("GUIDER_APERTURE = %v want 60", got)
	}
}

func TestGuideRateReported(t *testing.T) {
	d := mount.New("TestScope", func() (lx200.Mount, error) { return &fakeMount{}, nil },
		mount.WithGuideRate(0.75))
	var gr *server.Property
	for _, p := range d.Properties() {
		if p.Name == "GUIDE_RATE" {
			gr = p
		}
	}
	if gr == nil {
		t.Fatal("GUIDE_RATE not exposed")
	}
	if got := gr.Number("GUIDE_RATE_WE"); !approx(got, 0.75) {
		t.Errorf("GUIDE_RATE_WE = %v want 0.75", got)
	}
	if got := gr.Number("GUIDE_RATE_NS"); !approx(got, 0.75) {
		t.Errorf("GUIDE_RATE_NS = %v want 0.75", got)
	}
}

func TestGuideRateDefault(t *testing.T) {
	d := mount.New("TestScope", func() (lx200.Mount, error) { return &fakeMount{}, nil })
	for _, p := range d.Properties() {
		if p.Name == "GUIDE_RATE" && !approx(p.Number("GUIDE_RATE_WE"), 0.5) {
			t.Errorf("default guide rate = %v want 0.5", p.Number("GUIDE_RATE_WE"))
		}
	}
}

// guidingMount is a fakeMount that also reports a real guide rate (lx200.GuideRater).
type guidingMount struct {
	*fakeMount
	rate float64
}

func (g *guidingMount) GuideRateSidereal() (float64, error) { return g.rate, nil }

// TestGuideRateFromMount: a mount that can report its rate overrides the default on
// connect (tenmicron/am5 behaviour).
func TestGuideRateFromMount(t *testing.T) {
	gm := &guidingMount{fakeMount: &fakeMount{}, rate: 0.25}
	d := mount.New("TestScope", func() (lx200.Mount, error) { return gm, nil })
	d.HandleNew(&capPub{}, "CONNECTION", []server.NewMember{nm("CONNECT", "On")})
	for _, p := range d.Properties() {
		if p.Name == "GUIDE_RATE" && !approx(p.Number("GUIDE_RATE_WE"), 0.25) {
			t.Errorf("guide rate from mount = %v want 0.25 (the mount's real rate)", p.Number("GUIDE_RATE_WE"))
		}
	}
}

func TestNoOpticsNoTelescopeInfo(t *testing.T) {
	d := mount.New("TestScope", func() (lx200.Mount, error) { return &fakeMount{}, nil })
	for _, p := range d.Properties() {
		if p.Name == "TELESCOPE_INFO" {
			t.Error("TELESCOPE_INFO should be absent without optics")
		}
	}
}

func TestAbort(t *testing.T) {
	d, f := newDev()
	pub := &capPub{}
	d.HandleNew(pub, "TELESCOPE_ABORT_MOTION", []server.NewMember{nm("ABORT", "On")})
	if !f.snap().halted {
		t.Error("abort did not halt the mount")
	}
}

// TestEndToEndPulseGuide drives a pulse guide all the way over a TCP INDI session.
func TestEndToEndPulseGuide(t *testing.T) {
	f := &fakeMount{}
	d := mount.New("TestScope", func() (lx200.Mount, error) { return f, nil })

	s := server.New("127.0.0.1:0")
	if err := s.AddDevice(d); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.Serve(ctx) }()
	for i := 0; i < 200 && s.Addr() == nil; i++ {
		time.Sleep(time.Millisecond)
	}
	c, err := net.Dial("tcp", s.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	go drain(c) // consume the def/set stream the server pushes

	fmt.Fprint(c, `<newSwitchVector device="TestScope" name="CONNECTION"><oneSwitch name="CONNECT">On</oneSwitch></newSwitchVector>`)
	fmt.Fprint(c, `<newNumberVector device="TestScope" name="TELESCOPE_TIMED_GUIDE_NS"><oneNumber name="TIMED_GUIDE_N">512</oneNumber></newNumberVector>`)

	waitFor(t, func() bool { return len(f.snap().pulses) == 1 }, "pulse over the wire")
	if p := f.snap().pulses[0]; p.Dir != lx200.North || p.Ms != 512 {
		t.Errorf("pulse = %+v", p)
	}
}

func drain(c net.Conn) {
	buf := make([]byte, 4096)
	for {
		if _, err := c.Read(buf); err != nil {
			return
		}
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func approx(a, b float64) bool { d := a - b; return d < 1e-6 && d > -1e-6 }

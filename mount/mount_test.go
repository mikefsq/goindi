package mount_test

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mikefsq/goindi/mount"
	"github.com/mikefsq/goindi/server"
	"github.com/mikefsq/lx200"
)

// fakeMount is an lx200.Mount recording what the INDI device drives it to do.
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

// mountState is a lock-free snapshot of the recorded state.
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
func (c *capPub) messages() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.msgs...)
}

func nm(name, val string) server.NewMember { return server.NewMember{Name: name, Value: val} }

func newDev() (*mount.Device, *fakeMount) {
	f := &fakeMount{}
	return mount.New("TestScope", func() (lx200.Mount, error) { return f, nil }), f
}

// CONNECT settles the CONNECTION property Ok.
func TestConnect(t *testing.T) {
	d, _ := newDev()
	pub := &capPub{}
	d.HandleNew(pub, "CONNECTION", []server.NewMember{nm("CONNECT", "On")})
	for _, p := range d.Properties() {
		if p.Name == "CONNECTION" {
			if p.State() != server.Ok || !p.Switch("CONNECT") {
				t.Errorf("CONNECTION state=%v connect=%v", p.State(), p.Switch("CONNECT"))
			}
		}
	}
}

// With ON_COORD_SET=SLEW, a new coordinate slews rather than syncs.
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

// With ON_COORD_SET=SYNC, a new coordinate syncs rather than slews.
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

// A timed-guide vector reaches the mount as one pulse of the right direction and length.
func TestPulseGuide(t *testing.T) {
	d, f := newDev()
	pub := &capPub{}
	d.HandleNew(pub, "TELESCOPE_TIMED_GUIDE_NS", []server.NewMember{nm("TIMED_GUIDE_N", "512")})
	s := f.snap()
	if len(s.pulses) != 1 || s.pulses[0].Dir != lx200.North || s.pulses[0].Ms != 512 {
		t.Errorf("pulses = %+v", s.pulses)
	}
}

// A goto with a malformed number must alert the property and touch nothing on the mount.
func TestMalformedCoordinateRejected(t *testing.T) {
	d, f := newDev()
	pub := &capPub{}
	d.HandleNew(pub, "ON_COORD_SET", []server.NewMember{nm("SLEW", "On")})
	d.HandleNew(pub, "EQUATORIAL_EOD_COORD", []server.NewMember{nm("RA", "1O:23:45"), nm("DEC", "10")})
	time.Sleep(30 * time.Millisecond) // a slew, had one started, runs async
	s := f.snap()
	if s.slewed || s.synced || s.tRA != 0 || s.tDec != 0 {
		t.Errorf("malformed RA moved the mount: %+v", s)
	}
	for _, p := range d.Properties() {
		if p.Name == "EQUATORIAL_EOD_COORD" && p.State() != server.Alert {
			t.Errorf("EQUATORIAL_EOD_COORD state = %v, want Alert", p.State())
		}
	}
	found := false
	for _, m := range pub.messages() {
		if strings.Contains(m, "bad RA") {
			found = true
		}
	}
	if !found {
		t.Errorf("no rejection message published; got %v", pub.messages())
	}
}

// Coordinates in the INDI sexagesimal forms "H:M:S" and "D M S" are accepted.
func TestSexagesimalCoordinates(t *testing.T) {
	d, f := newDev()
	pub := &capPub{}
	d.HandleNew(pub, "ON_COORD_SET", []server.NewMember{nm("SLEW", "On")})
	d.HandleNew(pub, "EQUATORIAL_EOD_COORD", []server.NewMember{nm("RA", "12:30:00"), nm("DEC", "-5 30 00")})
	waitFor(t, func() bool { return f.snap().slewed }, "sexagesimal slew")
	s := f.snap()
	if !approx(s.tRA, 12.5) || !approx(s.tDec, -5.5) {
		t.Errorf("sexagesimal targets: tRA=%v tDec=%v want 12.5/-5.5", s.tRA, s.tDec)
	}
}

// A malformed duration must not pulse.
func TestMalformedGuidePulseRejected(t *testing.T) {
	d, f := newDev()
	pub := &capPub{}
	d.HandleNew(pub, "TELESCOPE_TIMED_GUIDE_NS", []server.NewMember{nm("TIMED_GUIDE_N", "12ms")})
	if got := f.snap().pulses; len(got) != 0 {
		t.Errorf("malformed duration pulsed the mount: %+v", got)
	}
	for _, p := range d.Properties() {
		if p.Name == "TELESCOPE_TIMED_GUIDE_NS" && p.State() != server.Alert {
			t.Errorf("TELESCOPE_TIMED_GUIDE_NS state = %v, want Alert", p.State())
		}
	}
}

// fakeOptics is a mutable Optics holder, in millimetres.
type fakeOptics struct {
	mu               sync.Mutex
	ap, fl, gap, gfl float64
}

func (o *fakeOptics) OpticsMM() (float64, float64, float64, float64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.ap, o.fl, o.gap, o.gfl
}

// WithOptics exposes TELESCOPE_INFO carrying the holder's values.
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

// WithGuideRate is reported on both GUIDE_RATE axes.
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

// Without WithGuideRate the reported rate defaults to 0.5x sidereal.
func TestGuideRateDefault(t *testing.T) {
	d := mount.New("TestScope", func() (lx200.Mount, error) { return &fakeMount{}, nil })
	for _, p := range d.Properties() {
		if p.Name == "GUIDE_RATE" && !approx(p.Number("GUIDE_RATE_WE"), 0.5) {
			t.Errorf("default guide rate = %v want 0.5", p.Number("GUIDE_RATE_WE"))
		}
	}
}

// guidingMount is a fakeMount that also reports its real guide rate.
type guidingMount struct {
	*fakeMount
	rate float64
}

func (g *guidingMount) GuideRateSidereal() (float64, error) { return g.rate, nil }

// A mount that can report its rate overrides the configured default on connect.
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

// dualAxisMount is a fakeMount that also supports dual-axis tracking.
type dualAxisMount struct {
	*fakeMount
	mu  sync.Mutex
	on  bool
	set []bool
}

func (m *dualAxisMount) DualAxisTracking() (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.on, nil
}

func (m *dualAxisMount) SetDualAxisTracking(on bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.on = on
	m.set = append(m.set, on)
	return nil
}

func (m *dualAxisMount) lastSet() (bool, bool) { // value, ok
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.set) == 0 {
		return false, false
	}
	return m.set[len(m.set)-1], true
}

// DUAL_AXIS_TRACKING appears only for a capable mount, seeded from its state, drives the
// mount on set, and is removed on disconnect.
func TestDualAxisTracking(t *testing.T) {
	plain, _ := newDev()
	plain.HandleNew(&capPub{}, "CONNECTION", []server.NewMember{nm("CONNECT", "On")})
	for _, p := range plain.Properties() {
		if p.Name == "DUAL_AXIS_TRACKING" {
			t.Error("DUAL_AXIS_TRACKING exposed for a mount without the capability")
		}
	}

	dm := &dualAxisMount{fakeMount: &fakeMount{}, on: true}
	d := mount.New("TestScope", func() (lx200.Mount, error) { return dm, nil })
	pub := &capPub{}
	d.HandleNew(pub, "CONNECTION", []server.NewMember{nm("CONNECT", "On")})

	var da *server.Property
	for _, p := range d.Properties() {
		if p.Name == "DUAL_AXIS_TRACKING" {
			da = p
		}
	}
	if da == nil {
		t.Fatal("DUAL_AXIS_TRACKING not exposed after connect")
	}
	if !da.Switch("ENABLE") || da.Switch("DISABLE") {
		t.Errorf("initial switch ENABLE=%v DISABLE=%v; want enabled (mount on)", da.Switch("ENABLE"), da.Switch("DISABLE"))
	}

	d.HandleNew(pub, "DUAL_AXIS_TRACKING", []server.NewMember{nm("DISABLE", "On")})
	if v, ok := dm.lastSet(); !ok || v != false {
		t.Errorf("SetDualAxisTracking(false) not issued (last=%v ok=%v)", v, ok)
	}
	if da.Switch("ENABLE") || !da.Switch("DISABLE") {
		t.Errorf("after disable: ENABLE=%v DISABLE=%v; want disabled", da.Switch("ENABLE"), da.Switch("DISABLE"))
	}

	d.HandleNew(pub, "CONNECTION", []server.NewMember{nm("DISCONNECT", "On")})
	for _, p := range d.Properties() {
		if p.Name == "DUAL_AXIS_TRACKING" {
			t.Error("DUAL_AXIS_TRACKING still exposed after disconnect")
		}
	}
}

// Without WithOptics there is no TELESCOPE_INFO.
func TestNoOpticsNoTelescopeInfo(t *testing.T) {
	d := mount.New("TestScope", func() (lx200.Mount, error) { return &fakeMount{}, nil })
	for _, p := range d.Properties() {
		if p.Name == "TELESCOPE_INFO" {
			t.Error("TELESCOPE_INFO should be absent without optics")
		}
	}
}

// TELESCOPE_ABORT_MOTION halts the mount.
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
	go drain(c) // the server blocks writing its def/set stream if nobody reads

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

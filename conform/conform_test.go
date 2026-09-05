package conform_test

import (
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mikefsq/goindi/ccd"
	"github.com/mikefsq/goindi/client"
	"github.com/mikefsq/goindi/conform"
	"github.com/mikefsq/goindi/mount"
	"github.com/mikefsq/goindi/server"
	"github.com/mikefsq/lx200"
)

type fakeMount struct {
	mu   sync.Mutex
	opMu sync.Mutex
	ra   float64
	dec  float64
}

func (f *fakeMount) RA() (float64, error)  { f.mu.Lock(); defer f.mu.Unlock(); return f.ra, nil }
func (f *fakeMount) Dec() (float64, error) { f.mu.Lock(); defer f.mu.Unlock(); return f.dec, nil }
func (f *fakeMount) SetTargetRA(h float64) (bool, error) {
	f.mu.Lock()
	f.ra = h
	f.mu.Unlock()
	return true, nil
}
func (f *fakeMount) SetTargetDec(d float64) (bool, error) {
	f.mu.Lock()
	f.dec = d
	f.mu.Unlock()
	return true, nil
}
func (f *fakeMount) SlewToTarget() error                   { return nil }
func (f *fakeMount) SyncToTarget() (string, error)         { return "ok", nil }
func (f *fakeMount) Halt() error                           { return nil }
func (f *fakeMount) Slewing() (bool, error)                { return false, nil }
func (f *fakeMount) Tracking() (bool, error)               { return true, nil }
func (f *fakeMount) SetTracking(bool) error                { return nil }
func (f *fakeMount) PulseGuide(lx200.Direction, int) error { return nil }
func (f *fakeMount) OpLock() func()                        { f.opMu.Lock(); return f.opMu.Unlock }

func serve(t *testing.T, devs ...server.Device) string {
	t.Helper()
	s := server.New("127.0.0.1:0")
	for _, d := range devs {
		if err := s.AddDevice(d); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.Serve(ctx) }()
	for i := 0; i < 200 && s.Addr() == nil; i++ {
		time.Sleep(time.Millisecond)
	}
	if s.Addr() == nil {
		t.Fatal("server did not start")
	}
	return s.Addr().String()
}

func dial(t *testing.T, addr string) *client.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := client.Dial(ctx, addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func runWith(t *testing.T, addr string, opts conform.Options) []conform.Result {
	t.Helper()
	c := dial(t, addr)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return conform.Run(ctx, c, opts)
}

func run(t *testing.T, addr string) []conform.Result {
	t.Helper()
	return runWith(t, addr, conform.Options{Mutate: true, Timeout: 2 * time.Second})
}

func TestMountDeviceConforms(t *testing.T) {
	f := &fakeMount{}
	d := mount.New("TestScope", func() (lx200.Mount, error) { return f, nil })
	addr := serve(t, d)

	results := run(t, addr)
	_, fail, _, _ := conform.Summarize(results)
	if fail != 0 {
		for _, r := range results {
			if r.Status == conform.Fail {
				t.Errorf("FAIL %s: %s — %s", r.Device, r.Check, r.Detail)
			}
		}
	}

	var sawNS, sawWE bool
	for _, r := range results {
		switch r.Check {
		case "pulse guide accepted: TELESCOPE_TIMED_GUIDE_NS":
			sawNS = true
		case "pulse guide accepted: TELESCOPE_TIMED_GUIDE_WE":
			sawWE = true
		}
	}
	if !sawNS {
		t.Error("no NS pulse guide check ran")
	}
	if !sawWE {
		t.Error("no WE pulse guide check ran")
	}
}

// brokenDev advertises the TELESCOPE interface but omits EQUATORIAL_EOD_COORD and gives
// CONNECTION the wrong permission.
type brokenDev struct{ props []*server.Property }

func newBrokenDev() *brokenDev {
	conn := server.ConnectionProperty("Broken")
	conn.Perm = server.RO // wrong: CONNECTION must be rw
	info := server.DriverInfoProperty("Broken", "Broken", "x", "1", server.InterfaceTelescope)
	return &brokenDev{props: []*server.Property{conn, info}} // no EQUATORIAL_EOD_COORD
}

func (b *brokenDev) Name() string                                           { return "Broken" }
func (b *brokenDev) Properties() []*server.Property                         { return b.props }
func (b *brokenDev) HandleNew(server.Publisher, string, []server.NewMember) {}

func TestConformerCatchesNonConformance(t *testing.T) {
	addr := serve(t, newBrokenDev())
	results := run(t, addr)

	var sawMissingEq, sawConnPerm bool
	for _, r := range results {
		if r.Status != conform.Fail {
			continue
		}
		if strings.Contains(r.Check, "EQUATORIAL_EOD_COORD") {
			sawMissingEq = true
		}
		if strings.Contains(r.Check, "CONNECTION is switch rw") {
			sawConnPerm = true
		}
	}
	if !sawMissingEq {
		t.Error("conformer did not flag missing EQUATORIAL_EOD_COORD")
	}
	if !sawConnPerm {
		t.Error("conformer did not flag CONNECTION wrong permission")
	}
}

// propsDev is a static-property device fixture.
type propsDev struct {
	name  string
	props []*server.Property
}

func (d *propsDev) Name() string                                           { return d.name }
func (d *propsDev) Properties() []*server.Property                         { return d.props }
func (d *propsDev) HandleNew(server.Publisher, string, []server.NewMember) {}

func TestNumberRangeConvention(t *testing.T) {
	num := func(name string, min, max float64) *server.Property {
		p := server.NewProperty("Ranges", name, server.NumberType, server.RO,
			&server.Member{Name: "V", Min: min, Max: max})
		p.SetState(server.Ok)
		return p
	}
	d := &propsDev{name: "Ranges", props: []*server.Property{
		server.ConnectionProperty("Ranges"),
		num("ZZ_BAD", 5, 0),
		num("AA_BAD", 2, 1),
		num("NO_LIMIT", 7, 7),
	}}
	addr := serve(t, d)
	results := runWith(t, addr, conform.Options{Timeout: 2 * time.Second})

	var fails []string
	for _, r := range results {
		if r.Status == conform.Fail && strings.HasPrefix(r.Check, "number range: ") {
			fails = append(fails, r.Check)
		}
		if r.Status == conform.Fail && strings.Contains(r.Check, "NO_LIMIT") {
			t.Errorf("min==max flagged as failure: %s — %s", r.Check, r.Detail)
		}
	}
	want := []string{"number range: AA_BAD.V", "number range: ZZ_BAD.V"}
	if len(fails) != len(want) {
		t.Fatalf("number range failures = %v, want %v", fails, want)
	}
	for i := range want {
		if fails[i] != want[i] {
			t.Errorf("failure[%d] = %q, want %q (report order not deterministic/sorted)", i, fails[i], want[i])
		}
	}
}

func TestInterfaceContractsSkippedNote(t *testing.T) {
	d := &propsDev{name: "NoInfo", props: []*server.Property{server.ConnectionProperty("NoInfo")}}
	addr := serve(t, d)
	results := runWith(t, addr, conform.Options{Timeout: 2 * time.Second})

	var sawWarn, sawSkip bool
	for _, r := range results {
		if r.Check == "DRIVER_INFO present" && r.Status == conform.Warn {
			sawWarn = true
		}
		if r.Check == "interface contracts skipped" {
			sawSkip = true
		}
	}
	if !sawWarn {
		t.Error("missing DRIVER_INFO did not WARN")
	}
	if !sawSkip {
		t.Error("no explicit 'interface contracts skipped' line for absent DRIVER_INTERFACE")
	}
}

// serveRaw serves hand-written INDI XML, so fixtures can violate rules the server
// package cannot express.
func serveRaw(t *testing.T, payload string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func(nc net.Conn) {
				defer nc.Close()
				_, _ = nc.Write([]byte(payload))
				_, _ = io.Copy(io.Discard, nc) // hold the conn open and drain client writes
			}(nc)
		}
	}()
	return ln.Addr().String()
}

func TestLightPermAndSwitchRule(t *testing.T) {
	addr := serveRaw(t, `
<defSwitchVector device='Proto' name='CONNECTION' state='Idle' perm='rw' rule='OneOfMany'>
 <defSwitch name='CONNECT'>Off</defSwitch>
 <defSwitch name='DISCONNECT'>On</defSwitch>
</defSwitchVector>
<defLightVector device='Proto' name='WEATHER_STATUS' state='Ok'>
 <defLight name='WEATHER_RAIN'>Ok</defLight>
</defLightVector>
<defSwitchVector device='Proto' name='NO_RULE' state='Idle' perm='rw'>
 <defSwitch name='X'>Off</defSwitch>
</defSwitchVector>
`)
	results := runWith(t, addr, conform.Options{Timeout: 2 * time.Second})

	var sawRuleFail bool
	for _, r := range results {
		if r.Status != conform.Fail {
			continue
		}
		if r.Check == "switch rule valid: NO_RULE" {
			sawRuleFail = true
		}
		if strings.Contains(r.Check, "WEATHER_STATUS") {
			t.Errorf("Light property flagged: %s — %s", r.Check, r.Detail)
		}
	}
	if !sawRuleFail {
		t.Error("Switch with no rule attribute not flagged")
	}
}

func TestRequestedDeviceNotFound(t *testing.T) {
	d := &propsDev{name: "Here", props: []*server.Property{server.ConnectionProperty("Here")}}
	addr := serve(t, d)
	results := runWith(t, addr, conform.Options{Device: "Nonexistent", Timeout: 500 * time.Millisecond})

	var saw bool
	for _, r := range results {
		if r.Status == conform.Fail && r.Check == "device not found" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("no 'device not found' FAIL; results: %v", results)
	}
}

func TestDeviceCountReported(t *testing.T) {
	a := &propsDev{name: "A", props: []*server.Property{server.ConnectionProperty("A")}}
	b := &propsDev{name: "B", props: []*server.Property{server.ConnectionProperty("B")}}
	addr := serve(t, a, b)
	results := runWith(t, addr, conform.Options{Timeout: 2 * time.Second})

	checked := map[string]bool{}
	for _, r := range results {
		if r.Check == "discovery" && !strings.HasPrefix(r.Detail, "2 device(s)") {
			t.Errorf("discovery detail = %q, want a \"2 device(s)\" prefix", r.Detail)
		}
		if r.Device != "" {
			checked[r.Device] = true
		}
	}
	if !checked["A"] || !checked["B"] {
		t.Errorf("checked devices = %v, want both A and B", checked)
	}
}

// lateDev defines its telescope properties only in response to CONNECT.
type lateDev struct {
	mu      sync.Mutex
	conn    *server.Property
	info    *server.Property
	extra   []*server.Property
	defined bool
	news    int // HandleNew invocations, any property
}

func newLateDev() *lateDev {
	const n = "LateScope"
	return &lateDev{
		conn: server.ConnectionProperty(n),
		info: server.DriverInfoProperty(n, n, "x", "1", server.InterfaceTelescope),
		extra: []*server.Property{
			server.EquatorialCoordProperty(n),
			server.OnCoordSetProperty(n),
			server.AbortProperty(n),
		},
	}
}

func (d *lateDev) Name() string { return "LateScope" }
func (d *lateDev) Properties() []*server.Property {
	d.mu.Lock()
	defer d.mu.Unlock()
	ps := []*server.Property{d.conn, d.info}
	if d.defined {
		ps = append(ps, d.extra...)
	}
	return ps
}
func (d *lateDev) HandleNew(pub server.Publisher, name string, members []server.NewMember) {
	d.mu.Lock()
	d.news++
	d.mu.Unlock()
	if name != "CONNECTION" {
		return
	}
	for _, m := range members {
		if m.On() {
			d.conn.SetSwitch(m.Name, true)
		}
	}
	d.conn.SetState(server.Ok)
	pub.Update(d.conn)
	if d.conn.Switch("CONNECT") {
		d.mu.Lock()
		first := !d.defined
		d.defined = true
		d.mu.Unlock()
		if first {
			for _, p := range d.extra {
				pub.Define(p)
			}
		}
	}
}
func (d *lateDev) handled() int { d.mu.Lock(); defer d.mu.Unlock(); return d.news }

func TestPostConnectContractsWithMutate(t *testing.T) {
	d := newLateDev()
	addr := serve(t, d)
	results := runWith(t, addr, conform.Options{Mutate: true, Timeout: 2 * time.Second})

	for _, r := range results {
		if r.Status == conform.Fail {
			t.Errorf("FAIL %s: %s — %s", r.Device, r.Check, r.Detail)
		}
	}
	var sawEq, sawDisc bool
	for _, r := range results {
		if r.Check == "EQUATORIAL_EOD_COORD type/perm" && r.Status == conform.Pass {
			sawEq = true
		}
		if r.Check == "DISCONNECT restores initial state" && r.Status == conform.Pass {
			sawDisc = true
		}
	}
	if !sawEq {
		t.Error("telescope contract not evaluated on the post-connect snapshot")
	}
	if !sawDisc {
		t.Error("conform did not disconnect the device it connected")
	}
	if d.conn.Switch("CONNECT") {
		t.Error("device left connected after the run")
	}
}

func TestPostConnectContractsSkippedWithoutMutate(t *testing.T) {
	d := newLateDev()
	addr := serve(t, d)
	results := runWith(t, addr, conform.Options{Timeout: 2 * time.Second})

	var sawSkip bool
	for _, r := range results {
		if r.Status == conform.Fail {
			t.Errorf("unexpected FAIL %s: %s — %s", r.Device, r.Check, r.Detail)
		}
		if r.Check == "interface contracts skipped" && strings.Contains(r.Detail, "-mutate") {
			sawSkip = true
		}
	}
	if !sawSkip {
		t.Error("no skip note pointing at -mutate for the unconnected device")
	}
	if n := d.handled(); n != 0 {
		t.Errorf("device received %d new*Vectors without -mutate; want 0", n)
	}
}

type fakeCam struct {
	mu    sync.Mutex
	ready bool
}

func (f *fakeCam) PixelSizeUm() (float64, float64) { return 5.86, 5.86 }
func (f *fakeCam) Size() (int, int)                { return 16, 8 }
func (f *fakeCam) BitsPerPixel() int               { return 16 }
func (f *fakeCam) StartExposure(float64) error {
	f.mu.Lock()
	f.ready = true
	f.mu.Unlock()
	return nil
}
func (f *fakeCam) ImageReady() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.ready }
func (f *fakeCam) Frame() (int, int, []byte, error) {
	w, h := 16, 8
	return w, h, make([]byte, w*h*2), nil
}
func (f *fakeCam) AbortExposure() error { f.mu.Lock(); f.ready = false; f.mu.Unlock(); return nil }

func TestCCDDeviceConforms(t *testing.T) {
	d := ccd.New("Cam", func() (ccd.Camera, error) { return &fakeCam{}, nil })
	addr := serve(t, d)
	results := runWith(t, addr, conform.Options{Mutate: true, Timeout: 2 * time.Second})

	for _, r := range results {
		if r.Status == conform.Fail && !strings.Contains(r.Check, "TELESCOPE_TIMED_GUIDE") {
			t.Errorf("FAIL %s: %s — %s", r.Device, r.Check, r.Detail)
		}
	}
	for _, w := range []string{
		"CCD_EXPOSURE type/perm", "CCD_INFO type/perm", "CCD_INFO members",
		"CCD BLOB vector present", "CCD exposure delivers FITS BLOB",
		"CCD BLOB size attr consistent",
	} {
		if !hasResult(results, w, conform.Pass) {
			t.Errorf("expected PASS %q not found", w)
		}
	}
}

// connectableDev is a static-property device whose CONNECTION works, so post-connect
// contract checks run against it.
type connectableDev struct {
	name  string
	conn  *server.Property
	props []*server.Property
}

func newConnectable(name string, iface int, props ...*server.Property) *connectableDev {
	return &connectableDev{
		name: name,
		conn: server.ConnectionProperty(name),
		props: append([]*server.Property{
			server.DriverInfoProperty(name, name, "x", "1", iface),
		}, props...),
	}
}

func (d *connectableDev) Name() string { return d.name }
func (d *connectableDev) Properties() []*server.Property {
	return append([]*server.Property{d.conn}, d.props...)
}
func (d *connectableDev) HandleNew(pub server.Publisher, name string, members []server.NewMember) {
	if name != "CONNECTION" {
		return
	}
	for _, m := range members {
		if m.On() {
			d.conn.SetSwitch(m.Name, true)
		}
	}
	d.conn.SetState(server.Ok)
	pub.Update(d.conn)
}

func TestBrokenCCDFails(t *testing.T) {
	exp := server.NewProperty("BadCam", "CCD_EXPOSURE", server.NumberType, server.RW,
		&server.Member{Name: "CCD_EXPOSURE_VALUE", Min: 0, Max: 3600})
	blob := server.NewProperty("BadCam", "CCD1", server.BLOBType, server.RO,
		&server.Member{Name: "CCD1"})
	blob.SetState(server.Ok)
	d := newConnectable("BadCam", server.InterfaceCCD, exp, blob)
	addr := serve(t, d)
	results := runWith(t, addr, conform.Options{Mutate: true, Timeout: time.Second})

	if !hasResult(results, "CCD_INFO present", conform.Fail) {
		t.Error("missing CCD_INFO not flagged")
	}
}

const multiIface = server.InterfaceFocuser | server.InterfaceFilter | server.InterfaceDome |
	server.InterfaceGPS | server.InterfaceWeather | server.InterfaceRotator

func multiProps(n string) []*server.Property {
	num := func(name string, perm server.Perm, members ...*server.Member) *server.Property {
		p := server.NewProperty(n, name, server.NumberType, perm, members...)
		p.SetState(server.Ok)
		return p
	}
	focus := num("ABS_FOCUS_POSITION", server.RW,
		&server.Member{Name: "FOCUS_ABSOLUTE_POSITION", Min: 0, Max: 10000})
	slot := num("FILTER_SLOT", server.RW,
		&server.Member{Name: "FILTER_SLOT_VALUE", Min: 1, Max: 8, Num: 1})
	geo := num("GEOGRAPHIC_COORD", server.RO,
		&server.Member{Name: "LAT", Min: -90, Max: 90},
		&server.Member{Name: "LONG", Min: 0, Max: 360},
		&server.Member{Name: "ELEV", Min: -200, Max: 10000})
	angle := num("ABS_ROTATOR_ANGLE", server.RW, &server.Member{Name: "ANGLE", Min: 0, Max: 360})
	dome := server.NewProperty(n, "DOME_MOTION", server.SwitchType, server.RW,
		&server.Member{Name: "DOME_CW"}, &server.Member{Name: "DOME_CCW"})
	dome.Rule = server.AtMostOne
	dome.SetState(server.Ok)
	utc := server.NewProperty(n, "TIME_UTC", server.TextType, server.RO,
		&server.Member{Name: "UTC"}, &server.Member{Name: "OFFSET"})
	utc.SetState(server.Ok)
	weather := server.NewProperty(n, "WEATHER_STATUS", server.LightType, server.RO,
		&server.Member{Name: "WEATHER_RAIN", Light: server.Ok})
	weather.SetState(server.Ok)
	return []*server.Property{focus, slot, dome, geo, utc, weather, angle}
}

func TestCompositeInterfaceContracts(t *testing.T) {
	d := newConnectable("Multi", multiIface, multiProps("Multi")...)
	addr := serve(t, d)
	results := runWith(t, addr, conform.Options{Mutate: true, Timeout: 2 * time.Second})

	for _, r := range results {
		if r.Status == conform.Fail {
			t.Errorf("FAIL %s: %s — %s", r.Device, r.Check, r.Detail)
		}
	}
	var ifaceDetail string
	for _, r := range results {
		if r.Check == "DRIVER_INTERFACE" {
			ifaceDetail = r.Detail
		}
	}
	for _, name := range []string{"FOCUSER", "FILTER", "DOME", "GPS", "WEATHER", "ROTATOR"} {
		if !strings.Contains(ifaceDetail, name) {
			t.Errorf("DRIVER_INTERFACE detail %q lacks %s", ifaceDetail, name)
		}
	}
	for _, w := range []string{
		"FOCUSER contract", "FILTER_SLOT type/perm", "DOME contract",
		"GEOGRAPHIC_COORD present", "TIME_UTC present",
		"WEATHER_STATUS is Light", "ABS_ROTATOR_ANGLE type/perm",
	} {
		if !hasResult(results, w, conform.Pass) {
			t.Errorf("expected PASS %q not found", w)
		}
	}
}

func TestCompositeBrokenContracts(t *testing.T) {
	d := newConnectable("MultiBad", multiIface)
	addr := serve(t, d)
	results := runWith(t, addr, conform.Options{Mutate: true, Timeout: 2 * time.Second})

	for _, w := range []string{
		"FOCUSER contract", "FILTER_SLOT present", "DOME contract",
		"GEOGRAPHIC_COORD present", "TIME_UTC present",
		"WEATHER_STATUS present", "ABS_ROTATOR_ANGLE present",
	} {
		if !hasResult(results, w, conform.Fail) {
			t.Errorf("expected FAIL %q not found", w)
		}
	}
}

func TestInterfaceListGeneral(t *testing.T) {
	d := newConnectable("Plain", 0)
	addr := serve(t, d)
	results := runWith(t, addr, conform.Options{Timeout: 2 * time.Second})

	found := false
	for _, r := range results {
		if r.Check == "DRIVER_INTERFACE" {
			found = true
			if r.Detail != "0 [GENERAL]" {
				t.Errorf("DRIVER_INTERFACE detail = %q, want \"0 [GENERAL]\"", r.Detail)
			}
		}
	}
	if !found {
		t.Error("no DRIVER_INTERFACE info line")
	}
}

func hasResult(results []conform.Result, check string, st conform.Status) bool {
	for _, r := range results {
		if r.Check == check && r.Status == st {
			return true
		}
	}
	return false
}

func TestRunHonorsContext(t *testing.T) {
	f := &fakeMount{}
	d := mount.New("TestScope", func() (lx200.Mount, error) { return f, nil })
	addr := serve(t, d)
	c := dial(t, addr)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already expired
	start := time.Now()
	results := conform.Run(ctx, c, conform.Options{Mutate: true, Timeout: 2 * time.Second})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Run took %v with an expired context; want prompt return", elapsed)
	}

	var aborted bool
	for _, r := range results {
		if r.Status == conform.Fail && strings.Contains(r.Detail, "aborted") {
			aborted = true
		}
	}
	if !aborted {
		t.Errorf("no aborted failure reported; results: %v", results)
	}
}

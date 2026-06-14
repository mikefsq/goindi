package conform_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mikefsq/goindi/client"
	"github.com/mikefsq/goindi/conform"
	"github.com/mikefsq/goindi/mount"
	"github.com/mikefsq/goindi/server"
	"github.com/mikefsq/lx200"
)

// --- a minimal lx200.Mount for the device under test ---

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

func run(t *testing.T, addr string) []conform.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := client.Dial(ctx, addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return conform.Run(c, conform.Options{Mutate: true, Timeout: 2 * time.Second})
}

// The real mount device must pass conformance cleanly (the "ConformU passes our
// driver" check).
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
}

// brokenDev advertises the TELESCOPE interface but omits EQUATORIAL_EOD_COORD and
// gives CONNECTION the wrong permission — the conformer must catch both.
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

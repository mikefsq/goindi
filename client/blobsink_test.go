package client_test

import (
	"io"
	"sync"
	"testing"
	"time"

	"github.com/mikefsq/goindi/client"
	"github.com/mikefsq/goindi/server"
)

// namedBlobDev is a BLOB-publishing device under a caller-chosen name, so one
// server can carry two of them.
type namedBlobDev struct {
	name  string
	props []*server.Property
	blob  *server.Property
}

func newNamedBlobDev(name string) *namedBlobDev {
	p := server.NewProperty(name, "CCD1", server.BLOBType, server.RO,
		&server.Member{Name: "CCD1", Label: "Image"})
	p.SetState(server.Ok)
	return &namedBlobDev{name: name, blob: p,
		props: []*server.Property{server.ConnectionProperty(name), p}}
}

func (b *namedBlobDev) Name() string                                           { return b.name }
func (b *namedBlobDev) Properties() []*server.Property                         { return b.props }
func (b *namedBlobDev) HandleNew(server.Publisher, string, []server.NewMember) {}

// enableBLOBs waits for a getProperties round trip after enabling delivery.
// The server processes both messages in order, so the reply confirms the policy
// is active before a test sends its BLOB.
func enableBLOBs(t *testing.T, c *client.Client, devs ...string) {
	t.Helper()
	for _, d := range devs {
		var rev uint64
		if p, ok := c.Property(d, "CCD1"); ok {
			rev = p.Rev
		}
		if err := c.EnableBLOB(d, "CCD1", client.BlobAlso); err != nil {
			t.Fatalf("EnableBLOB %s: %v", d, err)
		}
		if err := c.GetProperties(d, "CCD1"); err != nil {
			t.Fatalf("GetProperties %s: %v", d, err)
		}
		if _, err := c.WaitRev(d, "CCD1", rev, func(client.Property) bool { return true },
			5*time.Second); err != nil {
			t.Fatalf("%s never re-announced CCD1, so enableBLOB may not have been applied: %v", d, err)
		}
	}
}

func TestBlobSinkForKeepsTwoDevicesApart(t *testing.T) {
	a, b := newNamedBlobDev("CamA"), newNamedBlobDev("CamB")
	s := startServer(t, a, b)
	c := dialClient(t, s)
	if !c.WaitDevices(2, 2*time.Second) {
		t.Fatal("both devices never appeared")
	}
	enableBLOBs(t, c, "CamA", "CamB")

	var mu sync.Mutex
	got := map[string][]byte{}
	sink := func(dev string) func(client.BlobInfo) io.Writer {
		return func(info client.BlobInfo) io.Writer {
			return &collector{fn: func(p []byte) {
				mu.Lock()
				got[dev] = append(got[dev], p...)
				mu.Unlock()
			}}
		}
	}
	c.BlobSinkFor("CamA", sink("CamA"))
	c.BlobSinkFor("CamB", sink("CamB"))

	wantA, wantB := payload(), payload()
	wantB[0]++ // so a swap is visible, not just a length match
	s.SendBLOB("CamA", "CCD1", "CCD1", ".fits", wantA)
	s.SendBLOB("CamB", "CCD1", "CCD1", ".fits", wantB)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := len(got["CamA"]) == len(wantA) && len(got["CamB"]) == len(wantB)
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if !bytesEqual(got["CamA"], wantA) {
		t.Errorf("CamA got %d bytes, want %d (%v)", len(got["CamA"]), len(wantA),
			"a swap here means one camera is receiving the other's frames")
	}
	if !bytesEqual(got["CamB"], wantB) {
		t.Errorf("CamB got %d bytes, want %d", len(got["CamB"]), len(wantB))
	}
}

func TestBlobSinkForFallsBackToTheConnectionSink(t *testing.T) {
	a, b := newNamedBlobDev("CamA"), newNamedBlobDev("CamB")
	s := startServer(t, a, b)
	c := dialClient(t, s)
	if !c.WaitDevices(2, 2*time.Second) {
		t.Fatal("both devices never appeared")
	}
	enableBLOBs(t, c, "CamA", "CamB")

	var mu sync.Mutex
	var perDevice, shared int
	c.BlobSinkFor("CamA", func(client.BlobInfo) io.Writer {
		return &collector{fn: func(p []byte) { mu.Lock(); perDevice += len(p); mu.Unlock() }}
	})
	c.BlobSink(func(client.BlobInfo) io.Writer {
		return &collector{fn: func(p []byte) { mu.Lock(); shared += len(p); mu.Unlock() }}
	})

	want := payload()
	s.SendBLOB("CamA", "CCD1", "CCD1", ".fits", want)
	s.SendBLOB("CamB", "CCD1", "CCD1", ".fits", want)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := perDevice == len(want) && shared == len(want)
		mu.Unlock()
		if done {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Errorf("per-device sink got %d bytes and the connection sink %d; want %d each",
		perDevice, shared, len(want))
}

func TestBlobSinkForNilRemoves(t *testing.T) {
	a := newNamedBlobDev("CamA")
	s := startServer(t, a)
	c := dialClient(t, s)
	enableBLOBs(t, c, "CamA")

	var mu sync.Mutex
	var perDevice, shared int
	c.BlobSinkFor("CamA", func(client.BlobInfo) io.Writer {
		return &collector{fn: func(p []byte) { mu.Lock(); perDevice += len(p); mu.Unlock() }}
	})
	c.BlobSink(func(client.BlobInfo) io.Writer {
		return &collector{fn: func(p []byte) { mu.Lock(); shared += len(p); mu.Unlock() }}
	})
	c.BlobSinkFor("CamA", nil)

	want := payload()
	s.SendBLOB("CamA", "CCD1", "CCD1", ".fits", want)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := shared == len(want)
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if shared != len(want) || perDevice != 0 {
		t.Errorf("after removal: connection sink got %d, per-device %d; want %d and 0",
			shared, perDevice, len(want))
	}
}

type collector struct{ fn func([]byte) }

func (c *collector) Write(p []byte) (int, error) { c.fn(p); return len(p), nil }
func (c *collector) Close() error                { return nil }

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

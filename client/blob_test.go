package client_test

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/mikefsq/goindi/client"
	"github.com/mikefsq/goindi/server"
)

type blobDev struct{ props []*server.Property }

func newBlobDev() *blobDev {
	p := server.NewProperty("Cam", "CCD1", server.BLOBType, server.RO,
		&server.Member{Name: "CCD1", Label: "Image"})
	p.SetState(server.Ok)
	return &blobDev{props: []*server.Property{server.ConnectionProperty("Cam"), p}}
}

func (b *blobDev) Name() string                                           { return "Cam" }
func (b *blobDev) Properties() []*server.Property                         { return b.props }
func (b *blobDev) HandleNew(server.Publisher, string, []server.NewMember) {}

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
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("server never listened")
	return nil
}

func dialClient(t *testing.T, s *server.Server) *client.Client {
	t.Helper()
	c, err := client.Dial(context.Background(), s.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.GetProperties("", ""); err != nil {
		t.Fatal(err)
	}
	if !c.WaitDevices(1, 2*time.Second) {
		t.Fatal("no devices")
	}
	return c
}

// payload is bigger than one base64 quantum and not a multiple of three, so the
// decoder's padding and cross-chunk quad handling both matter.
func payload() []byte {
	b := make([]byte, 5000)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}

// TestBlobRoundTrip checks that an enabled BLOB reaches the sink byte-exact.
func TestBlobRoundTrip(t *testing.T) {
	s := startServer(t, newBlobDev())
	c := dialClient(t, s)

	var mu sync.Mutex
	var got []byte
	var info client.BlobInfo
	done := make(chan struct{})
	c.BufferBlobs(func(i client.BlobInfo, data []byte) {
		mu.Lock()
		info, got = i, data
		mu.Unlock()
		close(done)
	})
	if err := c.EnableBLOB("Cam", "CCD1", client.BlobAlso); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // the server must register the opt-in before the send

	want := payload()
	s.SendBLOB("Cam", "CCD1", "CCD1", ".fits", want)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("no BLOB delivered")
	}
	mu.Lock()
	defer mu.Unlock()
	if !bytes.Equal(got, want) {
		t.Fatalf("payload mismatch: got %d bytes, want %d", len(got), len(want))
	}
	if info.Device != "Cam" || info.Property != "CCD1" || info.Member != "CCD1" {
		t.Errorf("info identity = %+v", info)
	}
	if info.Format != ".fits" || info.Compressed {
		t.Errorf("info format = %q compressed=%v; want .fits, false", info.Format, info.Compressed)
	}
	if info.Size != int64(len(want)) {
		t.Errorf("info size = %d, want %d", info.Size, len(want))
	}

	// The payload is not retained on the property; its metadata is.
	if p, ok := c.Property("Cam", "CCD1"); !ok {
		t.Fatal("CCD1 property missing")
	} else if m, ok := p.Member("CCD1"); !ok {
		t.Error("CCD1 member missing — defBLOB was not decoded")
	} else if m.Size != int64(len(want)) {
		t.Errorf("member size = %d, want %d", m.Size, len(want))
	}
}

// TestBlobNotEnabled checks that no payload arrives without EnableBLOB.
func TestBlobNotEnabled(t *testing.T) {
	s := startServer(t, newBlobDev())
	c := dialClient(t, s)

	fired := make(chan struct{}, 1)
	c.BufferBlobs(func(client.BlobInfo, []byte) { fired <- struct{}{} })
	s.SendBLOB("Cam", "CCD1", "CCD1", ".fits", payload())

	select {
	case <-fired:
		t.Fatal("BLOB delivered without enableBLOB")
	case <-time.After(300 * time.Millisecond):
	}
}

// TestBlobSinkStreams checks that decoded bytes reach the sink in batches, one
// Write per chardata token rather than one per base64 quantum.
func TestBlobSinkStreams(t *testing.T) {
	s := startServer(t, newBlobDev())
	c := dialClient(t, s)

	var w countingWriter
	done := make(chan struct{})
	w.done = done
	c.BlobSink(func(client.BlobInfo) io.Writer { return &w })
	if err := c.EnableBLOB("Cam", "", client.BlobAlso); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)

	want := payload()
	s.SendBLOB("Cam", "CCD1", "CCD1", ".fits", want)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sink never closed")
	}
	if w.n != len(want) {
		t.Errorf("streamed %d bytes, want %d", w.n, len(want))
	}
	if maxWrites := len(want)/1024 + 2; w.writes < 1 || w.writes > maxWrites {
		t.Errorf("delivered in %d write(s), want between 1 and %d (batched per chardata token)", w.writes, maxWrites)
	}
	if !w.closed {
		t.Error("Close was not called to signal completion")
	}
}

type countingWriter struct {
	n      int
	writes int
	closed bool
	done   chan struct{}
}

func (w *countingWriter) Write(p []byte) (int, error) { w.n += len(p); w.writes++; return len(p), nil }
func (w *countingWriter) Close() error                { w.closed = true; close(w.done); return nil }

// TestBlobCompressedFormat checks that a ".z" payload reaches the sink
// compressed, with Size reporting the uncompressed count.
func TestBlobCompressedFormat(t *testing.T) {
	want := payload()
	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	zw.Write(want)
	zw.Close()
	enc := base64.StdEncoding.EncodeToString(zbuf.Bytes())

	const def = `<defBLOBVector device="Cam" name="CCD1" state="Ok" perm="ro">` +
		`<defBLOB name="CCD1" label="Image"/></defBLOBVector>`
	set := fmt.Sprintf(`<setBLOBVector device="Cam" name="CCD1" state="Ok">`+
		`<oneBLOB name="CCD1" size="%d" enclen="%d" format=".fits.z">%s</oneBLOB></setBLOBVector>`,
		len(want), len(enc), enc)

	addr := rawServer(t, def, set)
	c := dialRaw(t, addr)

	type frame struct {
		info client.BlobInfo
		data []byte
	}
	got := make(chan frame, 1)
	c.BufferBlobs(func(i client.BlobInfo, d []byte) { got <- frame{i, d} })

	select {
	case f := <-got:
		if f.info.Format != ".fits" || !f.info.Compressed {
			t.Errorf("format=%q compressed=%v; want .fits, true", f.info.Format, f.info.Compressed)
		}
		if f.info.Size != int64(len(want)) {
			t.Errorf("Size = %d, want the uncompressed count %d", f.info.Size, len(want))
		}
		if !bytes.Equal(f.data, zbuf.Bytes()) {
			t.Errorf("sink got %d bytes, want the %d compressed bytes verbatim", len(f.data), zbuf.Len())
		}
		zr, err := zlib.NewReader(bytes.NewReader(f.data))
		if err != nil {
			t.Fatalf("delivered bytes are not zlib: %v", err)
		}
		inflated, err := io.ReadAll(zr)
		if err != nil || !bytes.Equal(inflated, want) {
			t.Errorf("inflated %d bytes (err %v), want the original %d", len(inflated), err, len(want))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no BLOB delivered")
	}
	if n := c.SoftErrors(); n != 0 {
		t.Errorf("SoftErrors = %d, want 0 — a correct uncompressed size attr must pass the check", n)
	}
}

// TestLightMemberUpdates checks that a setLightVector moves the member, not just
// the vector state.
func TestLightMemberUpdates(t *testing.T) {
	const def = `<defLightVector device="Safety" name="SAFETY_STATUS" state="Ok">` +
		`<defLight name="SAFE" label="Safe">Ok</defLight></defLightVector>`
	const set = `<setLightVector device="Safety" name="SAFETY_STATUS" state="Alert">` +
		`<oneLight name="SAFE">Alert</oneLight></setLightVector>`

	addr := rawServer(t, def, set)
	c, err := client.Dial(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })

	got, ok := c.Wait("Safety", "SAFETY_STATUS", func(p client.Property) bool {
		m, _ := p.Member("SAFE")
		return m.Value == "Alert"
	}, 3*time.Second)
	if !ok {
		m, _ := got.Member("SAFE")
		t.Fatalf("light member never updated: value=%q state=%q", m.Value, got.State)
	}
	if got.State != "Alert" {
		t.Errorf("vector state = %q, want Alert", got.State)
	}
}

// rawServer writes each chunk in order, with a beat between, then holds the
// connection open.
func rawServer(t *testing.T, chunks ...string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		for _, c := range chunks {
			if _, err := io.WriteString(conn, c+"\n"); err != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		time.Sleep(3 * time.Second)
	}()
	return ln.Addr().String()
}

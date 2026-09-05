package client_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/mikefsq/goindi/client"
)

func dialRaw(t *testing.T, addr string) *client.Client {
	t.Helper()
	c, err := client.Dial(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func poll(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

func TestDelPropertyBookkeeping(t *testing.T) {
	const defP = `<defTextVector device="D" name="P" state="Ok" perm="ro">` +
		`<defText name="M" label="M">v</defText></defTextVector>`
	const del = `<delProperty device="D"/>`
	const defP2 = `<defTextVector device="D" name="P2" state="Ok" perm="ro">` +
		`<defText name="M" label="M">w</defText></defTextVector>`

	addr := rawServer(t, defP, defP, del, defP2) // define, redefine, delete, redefine
	c := dialRaw(t, addr)

	if _, ok := c.Wait("D", "P2", func(client.Property) bool { return true }, 3*time.Second); !ok {
		t.Fatal("P2 never defined after delete")
	}
	if devs := c.Devices(); len(devs) != 1 || devs[0] != "D" {
		t.Errorf("Devices() = %v, want exactly [D]", devs)
	}
	if _, ok := c.Property("D", "P"); ok {
		t.Error("P survived the device-wide delProperty")
	}
}

func TestMessageAttrs(t *testing.T) {
	const msg = `<message device="Cam" timestamp="2026-08-31T00:00:00" message="hello"/>`
	addr := rawServer(t, msg)
	c := dialRaw(t, addr)

	if !poll(t, 3*time.Second, func() bool { return len(c.Messages()) == 1 }) {
		t.Fatal("message never arrived")
	}
	m := c.Messages()[0]
	if m.Device != "Cam" || m.Timestamp != "2026-08-31T00:00:00" || m.Text != "hello" {
		t.Errorf("message = %+v", m)
	}
	if got, want := m.String(), "2026-08-31T00:00:00 Cam: hello"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestMessageRingCap(t *testing.T) {
	const n = 1010
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, `<message device="Cam" message="m%d"/>`, i)
	}
	addr := rawServer(t, b.String())
	c := dialRaw(t, addr)

	if !poll(t, 5*time.Second, func() bool {
		msgs := c.Messages()
		return len(msgs) == 1000 && msgs[999].Text == fmt.Sprintf("m%d", n-1)
	}) {
		msgs := c.Messages()
		t.Fatalf("log = %d messages, last %q; want 1000 ending m%d", len(msgs), msgs[len(msgs)-1].Text, n-1)
	}
	if first := c.Messages()[0].Text; first != fmt.Sprintf("m%d", n-1000) {
		t.Errorf("oldest retained = %q, want m%d", first, n-1000)
	}
}

func TestSetVectorMessage(t *testing.T) {
	const def = `<defNumberVector device="M" name="COORD" state="Ok" perm="rw">` +
		`<defNumber name="RA" label="RA" format="%f" min="0" max="24" step="0">0</defNumber></defNumberVector>`
	const set = `<setNumberVector device="M" name="COORD" state="Alert" message="mount fault">` +
		`<oneNumber name="RA">1</oneNumber></setNumberVector>`

	addr := rawServer(t, def, set)
	c := dialRaw(t, addr)

	p, ok := c.Wait("M", "COORD", func(p client.Property) bool { return p.State == "Alert" }, 3*time.Second)
	if !ok {
		t.Fatalf("COORD never went Alert (state=%q)", p.State)
	}
	if p.Message != "mount fault" {
		t.Errorf("Message = %q, want %q", p.Message, "mount fault")
	}
}

func TestZeroSizeBlobSkipsSink(t *testing.T) {
	const def = `<defBLOBVector device="Cam" name="CCD1" state="Ok" perm="ro">` +
		`<defBLOB name="X" label="X"/></defBLOBVector>`
	const setEmpty = `<setBLOBVector device="Cam" name="CCD1" state="Busy">` +
		`<oneBLOB name="X" size="0" enclen="0" format=".fits"></oneBLOB></setBLOBVector>`
	const setData = `<setBLOBVector device="Cam" name="CCD1" state="Ok">` +
		`<oneBLOB name="X" size="3" enclen="4" format=".fits">QUJD</oneBLOB></setBLOBVector>`

	addr := rawServer(t, def, setEmpty, setData)
	c := dialRaw(t, addr)

	got := make(chan []byte, 2)
	c.BufferBlobs(func(_ client.BlobInfo, data []byte) { got <- data })

	select {
	case data := <-got:
		if !bytes.Equal(data, []byte("ABC")) {
			t.Fatalf("first delivery = %q, want %q (a zero-size payload leaked through?)", data, "ABC")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("real payload never delivered")
	}
	// The sink fires before the vector's metadata is applied, hence the poll.
	if !poll(t, time.Second, func() bool { p, _ := c.Property("Cam", "CCD1"); return p.State == "Ok" }) {
		p, _ := c.Property("Cam", "CCD1")
		t.Errorf("state = %q, want Ok", p.State)
	}
}

func TestSoftErrorsCounted(t *testing.T) {
	const def = `<defTextVector device="D" name="P" state="Ok" perm="ro">` +
		`<defText name="M" label="M">v</defText></defTextVector>`
	const bad = `<setNumberVector device="D" name="P"><oneNumber name="M">1</wrong></setNumberVector>`

	addr := rawServer(t, def, bad)
	c := dialRaw(t, addr)
	logged := make(chan string, 4)
	c.Logf(func(format string, args ...any) { logged <- fmt.Sprintf(format, args...) })

	if _, ok := c.Wait("D", "P", func(client.Property) bool { return true }, 3*time.Second); !ok {
		t.Fatal("P never defined")
	}
	if !poll(t, 3*time.Second, func() bool { return c.SoftErrors() >= 1 }) {
		t.Fatal("SoftErrors never counted the malformed element")
	}
	select {
	case line := <-logged:
		if !strings.Contains(line, "setNumberVector") {
			t.Errorf("trace = %q, want it to name setNumberVector", line)
		}
	case <-time.After(time.Second):
		t.Error("Logf never saw the drop")
	}
}

func TestBlobSizeMismatch(t *testing.T) {
	const def = `<defBLOBVector device="Cam" name="CCD1" state="Ok" perm="ro">` +
		`<defBLOB name="X" label="X"/></defBLOBVector>`
	const set = `<setBLOBVector device="Cam" name="CCD1" state="Ok">` +
		`<oneBLOB name="X" size="10" enclen="4" format=".fits">QUJD</oneBLOB></setBLOBVector>`
	const after = `<defTextVector device="Cam" name="LATER" state="Ok" perm="ro">` +
		`<defText name="M" label="M">v</defText></defTextVector>`

	addr := rawServer(t, def, set, after)
	c := dialRaw(t, addr)
	got := make(chan []byte, 1)
	c.BufferBlobs(func(_ client.BlobInfo, data []byte) { got <- data })

	select {
	case data := <-got:
		if !bytes.Equal(data, []byte("ABC")) {
			t.Errorf("payload = %q, want %q", data, "ABC")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("payload never delivered")
	}
	if !poll(t, 3*time.Second, func() bool { return c.SoftErrors() >= 1 }) {
		t.Error("size mismatch never counted")
	}
	if _, ok := c.Wait("Cam", "LATER", func(client.Property) bool { return true }, 3*time.Second); !ok {
		t.Error("connection died on a size mismatch")
	}
}

type failingSink struct {
	aborted chan error
	closed  chan struct{}
}

func (f *failingSink) Write([]byte) (int, error)      { return 0, fmt.Errorf("disk full") }
func (f *failingSink) Close() error                   { close(f.closed); return nil }
func (f *failingSink) CloseWithError(err error) error { f.aborted <- err; return nil }

func TestBlobSinkCloseWithError(t *testing.T) {
	const def = `<defBLOBVector device="Cam" name="CCD1" state="Ok" perm="ro">` +
		`<defBLOB name="X" label="X"/></defBLOBVector>`
	const set = `<setBLOBVector device="Cam" name="CCD1" state="Ok">` +
		`<oneBLOB name="X" size="6" enclen="8" format=".fits">QUJDREVG</oneBLOB></setBLOBVector>`
	const after = `<defTextVector device="Cam" name="LATER" state="Ok" perm="ro">` +
		`<defText name="M" label="M">v</defText></defTextVector>`

	addr := rawServer(t, def, set, after)
	c := dialRaw(t, addr)
	sink := &failingSink{aborted: make(chan error, 1), closed: make(chan struct{})}
	c.BlobSink(func(client.BlobInfo) io.Writer { return sink })

	select {
	case err := <-sink.aborted:
		if err == nil || !strings.Contains(err.Error(), "disk full") {
			t.Errorf("CloseWithError(%v), want the write error", err)
		}
	case <-sink.closed:
		t.Fatal("Close called after a write error — payload passed off as complete")
	case <-time.After(3 * time.Second):
		t.Fatal("sink never finalised")
	}
	if !poll(t, 3*time.Second, func() bool { return c.SoftErrors() >= 1 }) {
		t.Error("sink write failure never counted")
	}
	if _, ok := c.Wait("Cam", "LATER", func(client.Property) bool { return true }, 3*time.Second); !ok {
		t.Error("connection died on a sink write error")
	}
}

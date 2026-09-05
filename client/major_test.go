package client_test

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/mikefsq/goindi/client"
)

// scriptedServer accepts one connection and hands it to script, which can
// interleave reads and writes.
func scriptedServer(t *testing.T, script func(net.Conn)) string {
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
		script(conn)
	}()
	return ln.Addr().String()
}

func readUntil(conn net.Conn, want string) bool {
	buf := make([]byte, 4096)
	var got strings.Builder
	for !strings.Contains(got.String(), want) {
		n, err := conn.Read(buf)
		if err != nil {
			return false
		}
		got.Write(buf[:n])
	}
	return true
}

func TestChardataTrimmed(t *testing.T) {
	const defT = "<defTextVector device='D' name='P' state='Ok' perm='ro'>\n" +
		"    <defText name='T' label='T'>\n      seed\n  </defText>\n" +
		"    <defText name='U' label='U'>\n      keep\n  </defText>\n" +
		"</defTextVector>"
	const defL = "<defLightVector device='D' name='L' state='Ok'>\n" +
		"    <defLight name='S' label='S'>\n      Idle\n  </defLight>\n" +
		"</defLightVector>"
	const setT = "<setTextVector device='D' name='P'>\n" +
		"    <oneText name='T'>\n      hello\n  </oneText>\n" +
		"</setTextVector>"
	const setL = "<setLightVector device='D' name='L'>\n" +
		"    <oneLight name='S'>\n      Alert\n  </oneLight>\n" +
		"</setLightVector>"

	addr := rawServer(t, defT, defL, setT, setL)
	c := dialRaw(t, addr)

	p, ok := c.Wait("D", "P", func(p client.Property) bool {
		m, _ := p.Member("T")
		return m.Value == "hello"
	}, 3*time.Second)
	if !ok {
		m, _ := p.Member("T")
		t.Fatalf("oneText value = %q, want %q (untrimmed chardata?)", m.Value, "hello")
	}
	if m, _ := p.Member("U"); m.Value != "keep" {
		t.Errorf("defText value = %q, want %q", m.Value, "keep")
	}
	lp, ok := c.Wait("D", "L", func(p client.Property) bool {
		m, _ := p.Member("S")
		return m.Value == "Alert"
	}, 3*time.Second)
	if !ok {
		m, _ := lp.Member("S")
		t.Fatalf("oneLight value = %q, want %q (untrimmed chardata?)", m.Value, "Alert")
	}
}

func TestSexagesimalOnWire(t *testing.T) {
	const def = `<defNumberVector device="M" name="EQUATORIAL_EOD_COORD" state="Ok" perm="rw">` +
		`<defNumber name="RA" label="RA" format="%10.6m" min="0" max="24" step="0">0:30:00</defNumber>` +
		`<defNumber name="DEC" label="DEC" format="%10.6m" min="-90" max="90" step="0">0</defNumber>` +
		`</defNumberVector>`
	const set = `<setNumberVector device="M" name="EQUATORIAL_EOD_COORD" state="Ok">` + "\n" +
		`<oneNumber name="RA">` + "\n      12:30:00\n" + `</oneNumber>` + "\n" +
		`<oneNumber name="DEC">-5 30 00</oneNumber>` + "\n" + `</setNumberVector>`

	addr := rawServer(t, def, set)
	c := dialRaw(t, addr)

	p, ok := c.Wait("M", "EQUATORIAL_EOD_COORD", func(p client.Property) bool {
		m, _ := p.Member("RA")
		return m.Num == 12.5
	}, 3*time.Second)
	if !ok {
		m, _ := p.Member("RA")
		t.Fatalf("RA Num = %v (value %q), want 12.5", m.Num, m.Value)
	}
	if m, _ := p.Member("DEC"); m.Num != -5.5 {
		t.Errorf("DEC Num = %v (value %q), want -5.5", m.Num, m.Value)
	}
}

func TestConnectionDeath(t *testing.T) {
	const def = `<defTextVector device="D" name="P" state="Ok" perm="ro">` +
		`<defText name="M" label="M">v</defText></defTextVector>`
	addr := scriptedServer(t, func(conn net.Conn) {
		if _, err := conn.Write([]byte(def + "\n")); err != nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
		// Returning here trips scriptedServer's deferred Close, killing the session.
	})
	c := dialRaw(t, addr)

	if _, ok := c.Wait("D", "P", func(client.Property) bool { return true }, 3*time.Second); !ok {
		t.Fatal("P never defined")
	}
	if err := c.Err(); err != nil {
		t.Errorf("Err() = %v while the connection is alive, want nil", err)
	}

	// This Wait is still pending when the server dies; it must not sit out its 10s.
	start := time.Now()
	if _, ok := c.Wait("D", "NEVER", func(client.Property) bool { return true }, 10*time.Second); ok {
		t.Error("Wait succeeded for a property that never existed")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("pending Wait took %v to notice the dead connection", elapsed)
	}
	select {
	case <-c.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Done() never closed after server death")
	}
	if c.Err() == nil {
		t.Error("Err() = nil after the read loop exited")
	}
}

func TestSetAndWaitIgnoresStaleOk(t *testing.T) {
	const def = `<defNumberVector device="M" name="COORD" state="Ok" perm="rw">` +
		`<defNumber name="RA" label="RA" format="%f" min="0" max="24" step="0">0</defNumber></defNumberVector>`
	const busy = `<setNumberVector device="M" name="COORD" state="Busy">` +
		`<oneNumber name="RA">0</oneNumber></setNumberVector>`
	const okSet = `<setNumberVector device="M" name="COORD" state="Ok">` +
		`<oneNumber name="RA">5</oneNumber></setNumberVector>`

	addr := scriptedServer(t, func(conn net.Conn) {
		if _, err := conn.Write([]byte(def + "\n")); err != nil {
			return
		}
		if !readUntil(conn, "newNumberVector") {
			return
		}
		conn.Write([]byte(busy + "\n"))
		time.Sleep(150 * time.Millisecond)
		conn.Write([]byte(okSet + "\n"))
		time.Sleep(3 * time.Second)
	})
	c := dialRaw(t, addr)
	if _, ok := c.Wait("M", "COORD", func(client.Property) bool { return true }, 3*time.Second); !ok {
		t.Fatal("COORD never defined")
	}

	start := time.Now()
	p, err := c.SetNumberAndWait("M", "COORD", map[string]float64{"RA": 5}, 5*time.Second)
	if err != nil {
		t.Fatalf("SetNumberAndWait: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("returned in %v — accepted the stale pre-command Ok", elapsed)
	}
	if m, _ := p.Member("RA"); m.Num != 5 {
		t.Errorf("RA = %v, want 5 (the post-command update)", m.Num)
	}
	if p.State != "Ok" {
		t.Errorf("state = %q, want Ok", p.State)
	}
	if p.Rev != 3 { // def, Busy, Ok
		t.Errorf("Rev = %d, want 3 (one per applied def/set)", p.Rev)
	}
}

func TestSetAndWaitAlert(t *testing.T) {
	const def = `<defNumberVector device="M" name="COORD" state="Ok" perm="rw">` +
		`<defNumber name="RA" label="RA" format="%f" min="0" max="24" step="0">0</defNumber></defNumberVector>`
	const alert = `<setNumberVector device="M" name="COORD" state="Alert" message="mount fault">` +
		`<oneNumber name="RA">0</oneNumber></setNumberVector>`

	addr := scriptedServer(t, func(conn net.Conn) {
		if _, err := conn.Write([]byte(def + "\n")); err != nil {
			return
		}
		if !readUntil(conn, "newNumberVector") {
			return
		}
		conn.Write([]byte(alert + "\n"))
		time.Sleep(3 * time.Second)
	})
	c := dialRaw(t, addr)
	if _, ok := c.Wait("M", "COORD", func(client.Property) bool { return true }, 3*time.Second); !ok {
		t.Fatal("COORD never defined")
	}

	p, err := c.SetNumberAndWait("M", "COORD", map[string]float64{"RA": 5}, 5*time.Second)
	if err == nil {
		t.Fatal("SetNumberAndWait succeeded on an Alert acknowledgement")
	}
	if !strings.Contains(err.Error(), "mount fault") {
		t.Errorf("error = %v, want it to carry the property message", err)
	}
	if p.State != "Alert" {
		t.Errorf("state = %q, want Alert", p.State)
	}
}

func TestCompressedBlobSizeChecked(t *testing.T) {
	raw := []byte("uncompressed original data, long enough to be worth compressing compressing compressing")
	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	zw.Write(raw)
	zw.Close()
	enc := base64.StdEncoding.EncodeToString(zbuf.Bytes())

	const def = `<defBLOBVector device="Cam" name="CCD1" state="Ok" perm="ro">` +
		`<defBLOB name="X" label="X"/></defBLOBVector>`
	// size attr lies: one byte more than the payload inflates to.
	badSize := fmt.Sprintf(`<setBLOBVector device="Cam" name="CCD1" state="Ok">`+
		`<oneBLOB name="X" size="%d" enclen="%d" format=".fits.z">%s</oneBLOB></setBLOBVector>`,
		len(raw)+1, len(enc), enc)
	notZlib := base64.StdEncoding.EncodeToString([]byte("this is not a zlib stream"))
	garbage := fmt.Sprintf(`<setBLOBVector device="Cam" name="CCD1" state="Ok">`+
		`<oneBLOB name="X" size="25" enclen="%d" format=".fits.z">%s</oneBLOB></setBLOBVector>`,
		len(notZlib), notZlib)
	const after = `<defTextVector device="Cam" name="LATER" state="Ok" perm="ro">` +
		`<defText name="M" label="M">v</defText></defTextVector>`

	addr := rawServer(t, def, badSize, garbage, after)
	c := dialRaw(t, addr)
	logged := make(chan string, 8)
	c.Logf(func(format string, args ...any) { logged <- fmt.Sprintf(format, args...) })
	delivered := make(chan []byte, 2)
	c.BufferBlobs(func(_ client.BlobInfo, data []byte) { delivered <- data })

	for i, want := range [][]byte{zbuf.Bytes(), []byte("this is not a zlib stream")} {
		select {
		case data := <-delivered:
			if !bytes.Equal(data, want) {
				t.Errorf("payload %d: got %d bytes, want %d", i, len(data), len(want))
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("payload %d never delivered", i)
		}
	}
	if !poll(t, 3*time.Second, func() bool { return c.SoftErrors() >= 2 }) {
		t.Errorf("SoftErrors = %d, want 2 (wrong size, non-zlib payload)", c.SoftErrors())
	}
	var sawMismatch, sawInflate bool
	for done := false; !done; {
		select {
		case line := <-logged:
			sawMismatch = sawMismatch || strings.Contains(line, "size attr says")
			sawInflate = sawInflate || strings.Contains(line, "does not inflate")
		default:
			done = true
		}
	}
	if !sawMismatch || !sawInflate {
		t.Errorf("traces: mismatch=%v inflate=%v, want both", sawMismatch, sawInflate)
	}
	if _, ok := c.Wait("Cam", "LATER", func(client.Property) bool { return true }, 3*time.Second); !ok {
		t.Error("connection died on a compressed size mismatch")
	}
}

package client

import (
	"bytes"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// TestDecodeQuad checks the base64 quantum decoder, padding included.
func TestDecodeQuad(t *testing.T) {
	cases := []struct {
		in   string
		want string // "" means an error is expected
	}{
		{"QUJD", "ABC"},
		{"QUI=", "AB"},
		{"QQ==", "A"},
		{"Q===", ""}, // padding in position 1
		{"=QQQ", ""}, // padding in position 0
		{"QQ=Q", ""}, // data after padding
		{"QU!D", ""}, // byte outside the alphabet
	}
	for _, tc := range cases {
		var quad [4]byte
		var out [3]byte
		copy(quad[:], tc.in)
		n, err := decodeQuad(&quad, &out)
		if tc.want == "" {
			if err == nil {
				t.Errorf("decodeQuad(%q) = %q, want error", tc.in, out[:n])
			}
			continue
		}
		if err != nil {
			t.Errorf("decodeQuad(%q): %v", tc.in, err)
			continue
		}
		if got := string(out[:n]); got != tc.want {
			t.Errorf("decodeQuad(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestB64WriterRejectsDataAfterPadding checks that a padded quantum ends the
// payload.
func TestB64WriterRejectsDataAfterPadding(t *testing.T) {
	var buf bytes.Buffer
	w := &b64Writer{w: &buf}
	if err := w.Write([]byte("QQ==QUJD")); err == nil {
		t.Error("data after a padded quantum accepted")
	}
}

// TestB64WriterCountsDecodedBytes checks the decoded byte count across tokens.
func TestB64WriterCountsDecodedBytes(t *testing.T) {
	var buf bytes.Buffer
	w := &b64Writer{w: &buf}
	if err := w.Write([]byte("QUJD\nQUI=")); err != nil {
		t.Fatal(err)
	}
	if err := w.finish(); err != nil {
		t.Fatal(err)
	}
	if w.total != 5 || buf.String() != "ABCAB" {
		t.Errorf("total=%d out=%q, want 5 %q", w.total, buf.String(), "ABCAB")
	}
}

// TestSendWriteDeadline checks that a peer which stops reading turns send into a
// timeout error rather than a hang.
func TestSendWriteDeadline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			accepted <- conn // held open and never read, so the peer's buffers fill
		}
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	t.Cleanup(func() {
		select {
		case c := <-accepted:
			c.Close()
		default:
		}
	})

	old := writeTimeout
	writeTimeout = 100 * time.Millisecond
	defer func() { writeTimeout = old }()

	c := &Client{conn: conn, store: map[string]map[string]*Property{}}
	big := map[string]string{"M": strings.Repeat("x", 1<<16)}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) { // fill the socket buffers until a write blocks
		if err = c.SetText("D", "P", big); err != nil {
			break
		}
	}
	if err == nil {
		t.Fatal("send never timed out against a non-reading peer")
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Errorf("send error = %v, want a timeout", err)
	}
}

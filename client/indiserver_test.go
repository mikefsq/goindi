//go:build integration

package client_test

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/mikefsq/goindi/client"
)

const simCCDDevice = "CCD Simulator"

func indiBuild(t *testing.T) string {
	t.Helper()
	root := os.Getenv("GOINDI_INDI_BUILD")
	if root == "" {
		root = filepath.Join(os.Getenv("HOME"), "indihurd", "build", "indi")
	}
	if _, err := os.Stat(filepath.Join(root, "indiserver", "indiserver")); err != nil {
		t.Skipf("no indiserver under %s (set GOINDI_INDI_BUILD): %v", root, err)
	}
	return root
}

// startIndiserver runs a real indiserver on a free port with the given drivers.
func startIndiserver(t *testing.T, drivers ...string) string {
	t.Helper()
	root := indiBuild(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // indiserver rebinds the port; racing for it beats a fixed port

	args := []string{"-p", fmt.Sprint(port)}
	args = append(args, drivers...)
	cmd := exec.Command(filepath.Join(root, "indiserver", "indiserver"), args...)
	cmd.Env = append(os.Environ(),
		"LD_LIBRARY_PATH="+filepath.Join(filepath.Dir(root), "prefix", "lib64"),
		"HOME="+t.TempDir()) // drivers persist config under ~/.indi; keep runs independent
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if testing.Verbose() {
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start indiserver: %v", err)
	}
	t.Cleanup(func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		cmd.Wait()
	})

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for i := 0; i < 100; i++ {
		if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			c.Close()
			return addr
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("indiserver never accepted on %s", addr)
	return ""
}

func connectDevice(t *testing.T, c *client.Client, device string) {
	t.Helper()
	if _, ok := c.Wait(device, "CONNECTION", func(client.Property) bool { return true }, 10*time.Second); !ok {
		t.Fatalf("%s never defined CONNECTION", device)
	}
	if err := c.SetSwitch(device, "CONNECTION", map[string]bool{"CONNECT": true, "DISCONNECT": false}); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Wait(device, "CONNECTION", func(p client.Property) bool {
		m, _ := p.Member("CONNECT")
		return m.On
	}, 15*time.Second); !ok {
		t.Fatalf("%s never connected", device)
	}
}

func TestAgainstIndiserver(t *testing.T) {
	root := indiBuild(t)
	addr := startIndiserver(t, filepath.Join(root, "drivers", "ccd", "indi_simulator_ccd"))

	c, err := client.Dial(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.GetProperties("", ""); err != nil {
		t.Fatal(err)
	}
	if !c.WaitDevices(1, 10*time.Second) {
		t.Fatal("indiserver announced no devices")
	}
	if devs := c.Devices(); devs[0] != simCCDDevice {
		t.Fatalf("device = %q, want %q", devs[0], simCCDDevice)
	}
	connectDevice(t, c, simCCDDevice)

	// Post-connect properties arrive in a burst after CONNECTION goes Ok.
	if _, ok := c.Wait(simCCDDevice, "CCD_EXPOSURE", func(client.Property) bool { return true }, 15*time.Second); !ok {
		t.Fatal("CCD_EXPOSURE never appeared")
	}

	type frame struct {
		info client.BlobInfo
		data []byte
	}
	got := make(chan frame, 1)
	c.BufferBlobs(func(i client.BlobInfo, d []byte) {
		select {
		case got <- frame{i, d}:
		default:
		}
	})
	if err := c.EnableBLOB(simCCDDevice, "", client.BlobAlso); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // let the opt-in land before exposing

	if err := c.SetNumber(simCCDDevice, "CCD_EXPOSURE", map[string]float64{"CCD_EXPOSURE_VALUE": 0.2}); err != nil {
		t.Fatal(err)
	}

	select {
	case f := <-got:
		if len(f.data) == 0 {
			t.Fatal("empty frame")
		}
		// A FITS primary header opens with this card, so anything else means the
		// base64 decode is wrong.
		if !bytes.HasPrefix(f.data, []byte("SIMPLE  =")) {
			t.Errorf("payload is not FITS: first 32 bytes %q", f.data[:min(32, len(f.data))])
		}
		if f.info.Format != ".fits" {
			t.Errorf("format = %q, want .fits", f.info.Format)
		}
		if f.info.Size != 0 && f.info.Size != int64(len(f.data)) {
			t.Errorf("declared size %d, decoded %d", f.info.Size, len(f.data))
		}
		t.Logf("received %d-byte %s frame from %s", len(f.data), f.info.Format, f.info.Device)
	case <-time.After(30 * time.Second):
		t.Fatal("no frame — the exposure produced no BLOB")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

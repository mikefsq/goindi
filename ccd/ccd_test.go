package ccd

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mikefsq/goindi/server"
)

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

type capPub struct {
	mu         sync.Mutex
	blobs      int
	lastFormat string
	lastData   []byte
}

func (c *capPub) Define(*server.Property) {}
func (c *capPub) Update(*server.Property) {}
func (c *capPub) Message(string, string)  {}
func (c *capPub) Delete(string, string)   {}
func (c *capPub) SendBLOB(_, _, _, format string, data []byte) {
	c.mu.Lock()
	c.blobs, c.lastFormat, c.lastData = c.blobs+1, format, data
	c.mu.Unlock()
}
func (c *capPub) count() int { c.mu.Lock(); defer c.mu.Unlock(); return c.blobs }

func nm(name, val string) server.NewMember { return server.NewMember{Name: name, Value: val} }

func TestCCDConnectReportsPixelSize(t *testing.T) {
	d := New("Cam", func() (Camera, error) { return &fakeCam{}, nil })
	d.HandleNew(&capPub{}, "CONNECTION", []server.NewMember{nm("CONNECT", "On")})
	for _, p := range d.Properties() {
		if p.Name == "CCD_INFO" {
			if p.Number("CCD_PIXEL_SIZE") != 5.86 {
				t.Errorf("CCD_PIXEL_SIZE = %v want 5.86", p.Number("CCD_PIXEL_SIZE"))
			}
			if p.Number("CCD_BITSPERPIXEL") != 16 {
				t.Errorf("CCD_BITSPERPIXEL = %v want 16", p.Number("CCD_BITSPERPIXEL"))
			}
		}
	}
}

func TestCCDExposureDeliversFITSBlob(t *testing.T) {
	d := New("Cam", func() (Camera, error) { return &fakeCam{}, nil })
	pub := &capPub{}
	d.HandleNew(pub, "CCD_EXPOSURE", []server.NewMember{nm("CCD_EXPOSURE_VALUE", "0.01")})

	for i := 0; i < 250 && pub.count() == 0; i++ {
		time.Sleep(2 * time.Millisecond)
	}
	if pub.count() != 1 {
		t.Fatalf("expected 1 BLOB, got %d", pub.count())
	}
	if pub.lastFormat != ".fits" {
		t.Errorf("format = %q want .fits", pub.lastFormat)
	}
	if !strings.HasPrefix(string(pub.lastData), "SIMPLE") {
		t.Error("BLOB is not a FITS file (no SIMPLE keyword)")
	}
	if len(pub.lastData)%2880 != 0 {
		t.Errorf("FITS length %d not 2880-aligned", len(pub.lastData))
	}
}

func TestEncodeFITS(t *testing.T) {
	out16 := encodeFITS(16, 8, 16, make([]byte, 16*8*2))
	if len(out16)%2880 != 0 || !strings.Contains(string(out16[:2880]), "NAXIS1") {
		t.Errorf("16-bit FITS malformed (len %d)", len(out16))
	}
	if !strings.Contains(string(out16[:2880]), "BITPIX  =                   16") {
		t.Error("16-bit FITS should be BITPIX 16")
	}
	out8 := encodeFITS(16, 8, 8, make([]byte, 16*8))
	if len(out8)%2880 != 0 || !strings.Contains(string(out8[:2880]), "BITPIX  =                    8") {
		t.Errorf("8-bit FITS malformed (len %d)", len(out8))
	}
}

package ccd

import (
	"testing"

	"github.com/mikefsq/goindi/server"
)

// capCam is a fakeCam that also supports gain, offset, and subframe ROI.
type capCam struct {
	fakeCam
	gain, off  int
	x, y, w, h int
}

func (c *capCam) Gain() (int, int, int)   { return c.gain, 0, 600 }
func (c *capCam) SetGain(n int) error     { c.gain = n; return nil }
func (c *capCam) Offset() (int, int, int) { return c.off, 0, 255 }
func (c *capCam) SetOffset(n int) error   { c.off = n; return nil }
func (c *capCam) Subframe() (int, int, int, int) { return c.x, c.y, c.w, c.h }
func (c *capCam) SetSubframe(x, y, w, h int) error {
	c.x, c.y, c.w, c.h = x, y, w, h
	return nil
}

func TestCCDGainAndSubframeCaps(t *testing.T) {
	cam := &capCam{gain: 100, off: 10, x: 0, y: 0, w: 16, h: 8}
	d := New("Cam", func() (Camera, error) { return cam, nil })
	pub := &capPub{}

	if d.controls != nil || d.frame != nil {
		t.Fatal("caps advertised before connect")
	}

	d.HandleNew(pub, "CONNECTION", []server.NewMember{nm("CONNECT", "On")})
	if d.controls == nil {
		t.Fatal("CCD_CONTROLS not defined on connect")
	}
	if d.frame == nil {
		t.Fatal("CCD_FRAME not defined on connect")
	}

	// Gain set over INDI routes to the camera and reads back.
	d.HandleNew(pub, "CCD_CONTROLS", []server.NewMember{nm("Gain", "350"), nm("Offset", "20")})
	if cam.gain != 350 || cam.off != 20 {
		t.Errorf("controls not routed: gain=%d off=%d", cam.gain, cam.off)
	}
	if g := d.controls.Number("Gain"); g != 350 {
		t.Errorf("CCD_CONTROLS Gain readback = %v, want 350", g)
	}

	// Subframe set over INDI routes to the camera and reads back.
	d.HandleNew(pub, "CCD_FRAME", []server.NewMember{
		nm("X", "2"), nm("Y", "1"), nm("WIDTH", "8"), nm("HEIGHT", "4"),
	})
	if cam.x != 2 || cam.y != 1 || cam.w != 8 || cam.h != 4 {
		t.Errorf("subframe not routed: %d,%d,%d,%d", cam.x, cam.y, cam.w, cam.h)
	}
	if w := d.frame.Number("WIDTH"); w != 8 {
		t.Errorf("CCD_FRAME WIDTH readback = %v, want 8", w)
	}
}

// A camera without the optional capabilities must not advertise CCD_CONTROLS/CCD_FRAME.
func TestCCDNoCapsWhenUnsupported(t *testing.T) {
	d := New("Cam", func() (Camera, error) { return &fakeCam{}, nil })
	d.HandleNew(&capPub{}, "CONNECTION", []server.NewMember{nm("CONNECT", "On")})
	if d.controls != nil || d.frame != nil {
		t.Error("plain camera advertised gain/subframe properties")
	}
}

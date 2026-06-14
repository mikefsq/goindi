// Package ccd is a generic INDI camera (CCD) device over any frame source. It
// exposes the standard camera properties PHD2 / Ekos drive — CCD_INFO (pixel size,
// dimensions), CCD_EXPOSURE, and the CCD1 BLOB carrying each frame as FITS — and it
// advertises the CCD+GUIDER interface so PHD2 lists it as a guide camera.
//
// It delivers RAW frames with no pixel transformation (no debayer, bin, or stretch):
// the client does its own centroiding on the unmodified sensor data, so any
// transformation here would wreck guiding.
package ccd

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	"github.com/mikefsq/goindi/server"
)

// Camera is the frame source the device drives — the fleet adapts a concrete camera
// (a sim camera, later astrocam) to it.
type Camera interface {
	PixelSizeUm() (x, y float64)                 // pixel pitch, microns
	Size() (w, h int)                            // sensor pixels
	BitsPerPixel() int                           // e.g. 16
	StartExposure(seconds float64) error         // begin an exposure
	ImageReady() bool                            // frame available?
	Frame() (w, h int, pixels []byte, err error) // RAW little-endian, BitsPerPixel deep
	AbortExposure() error
}

// CameraFunc returns the live camera, or an error if not connected (called per
// operation so a reconnect on the owning side is transparent).
type CameraFunc func() (Camera, error)

// Device is the INDI CCD adapter. Build it with New and register it with a server.Server.
type Device struct {
	name string
	cam  CameraFunc
	poll time.Duration

	conn       *server.Property
	driverInfo *server.Property
	info       *server.Property // CCD_INFO
	exposure   *server.Property // CCD_EXPOSURE
	abort      *server.Property // CCD_ABORT_EXPOSURE
	binning    *server.Property // CCD_BINNING
	blob       *server.Property // CCD1 (BLOB definition)

	mu        sync.Mutex
	connected bool
	exposing  bool
	ctx       context.Context
}

// New builds the device named name (the INDI device id clients pick) over cam.
func New(name string, cam CameraFunc) *Device {
	d := &Device{name: name, cam: cam, poll: 50 * time.Millisecond}
	d.conn = server.ConnectionProperty(name)
	d.driverInfo = server.DriverInfoProperty(name, name, "goindi-ccd", "1.0",
		server.InterfaceCCD|server.InterfaceGuider)
	d.info = ccdInfoProperty(name)
	d.exposure = exposureProperty(name)
	d.abort = abortProperty(name)
	d.binning = binningProperty(name)
	d.blob = blobProperty(name)
	if c, err := cam(); err == nil { // seed CCD_INFO so the initial def carries pixel size
		d.fillInfo(c)
	}
	return d
}

func (d *Device) Name() string { return d.name }

func (d *Device) Properties() []*server.Property {
	return []*server.Property{d.conn, d.driverInfo, d.info, d.exposure, d.abort, d.binning, d.blob}
}

func (d *Device) HandleNew(pub server.Publisher, name string, members []server.NewMember) {
	switch name {
	case "CONNECTION":
		d.handleConnection(pub, members)
	case "CCD_EXPOSURE":
		for _, m := range members {
			if m.Name == "CCD_EXPOSURE_VALUE" {
				d.startExposure(pub, m.Float())
			}
		}
	case "CCD_ABORT_EXPOSURE":
		for _, m := range members {
			if m.Name == "ABORT" && m.On() {
				if c, err := d.cam(); err == nil {
					_ = c.AbortExposure()
				}
				d.setExposing(false)
				d.exposure.SetState(server.Idle)
				pub.Update(d.exposure)
			}
		}
		d.abort.SetSwitch("ABORT", false)
		d.abort.SetState(server.Ok)
		pub.Update(d.abort)
	case "CCD_BINNING":
		for _, m := range members {
			d.binning.SetNumber(m.Name, m.Float())
		}
		d.binning.SetState(server.Ok)
		pub.Update(d.binning)
	}
}

func (d *Device) handleConnection(pub server.Publisher, members []server.NewMember) {
	connect := false
	for _, m := range members {
		switch m.Name {
		case "CONNECT":
			connect = m.On()
		case "DISCONNECT":
			if m.On() {
				connect = false
			}
		}
	}
	if connect {
		c, err := d.cam()
		if err != nil {
			d.conn.SetState(server.Alert)
			pub.Update(d.conn)
			pub.Message(d.name, "connect failed: "+err.Error())
			return
		}
		d.setConnected(true)
		d.conn.SetSwitch("CONNECT", true)
		d.fillInfo(c)
		pub.Update(d.info) // PHD2 reads CCD_INFO here for the pixel size
	} else {
		d.setConnected(false)
		d.conn.SetSwitch("DISCONNECT", true)
	}
	d.conn.SetState(server.Ok)
	pub.Update(d.conn)
}

// fillInfo copies the camera's geometry into CCD_INFO.
func (d *Device) fillInfo(c Camera) {
	px, py := c.PixelSizeUm()
	w, h := c.Size()
	d.info.SetNumber("CCD_MAX_X", float64(w))
	d.info.SetNumber("CCD_MAX_Y", float64(h))
	d.info.SetNumber("CCD_PIXEL_SIZE", px)
	d.info.SetNumber("CCD_PIXEL_SIZE_X", px)
	d.info.SetNumber("CCD_PIXEL_SIZE_Y", py)
	d.info.SetNumber("CCD_BITSPERPIXEL", float64(c.BitsPerPixel()))
}

// startExposure begins an exposure and, off the read loop, awaits the frame and
// pushes it as a BLOB.
func (d *Device) startExposure(pub server.Publisher, secs float64) {
	c, err := d.cam()
	if err != nil {
		d.exposure.SetState(server.Alert)
		pub.Update(d.exposure)
		return
	}
	if err := c.StartExposure(secs); err != nil {
		d.exposure.SetState(server.Alert)
		pub.Update(d.exposure)
		pub.Message(d.name, "expose: "+err.Error())
		return
	}
	d.setExposing(true)
	d.exposure.SetNumber("CCD_EXPOSURE_VALUE", secs)
	d.exposure.SetState(server.Busy)
	pub.Update(d.exposure)
	go d.awaitFrame(pub, c)
}

func (d *Device) awaitFrame(pub server.Publisher, c Camera) {
	ctx := d.exposeCtx()
	tick := time.NewTicker(d.poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if !d.isExposing() {
				return // aborted
			}
			if !c.ImageReady() {
				continue
			}
			w, h, px, err := c.Frame()
			if err != nil {
				d.setExposing(false)
				d.exposure.SetState(server.Alert)
				pub.Update(d.exposure)
				pub.Message(d.name, "frame: "+err.Error())
				return
			}
			// RAW frame straight to the BLOB — no transformation.
			pub.SendBLOB(d.name, "CCD1", "CCD1", ".fits", encodeFITS(w, h, c.BitsPerPixel(), px))
			d.setExposing(false)
			d.exposure.SetNumber("CCD_EXPOSURE_VALUE", 0)
			d.exposure.SetState(server.Ok)
			pub.Update(d.exposure)
			return
		}
	}
}

// Start records the server context so an in-flight exposure goroutine stops on shutdown.
func (d *Device) Start(ctx context.Context, _ server.Publisher) {
	d.mu.Lock()
	d.ctx = ctx
	d.mu.Unlock()
}

func (d *Device) setConnected(b bool) { d.mu.Lock(); d.connected = b; d.mu.Unlock() }
func (d *Device) setExposing(b bool)  { d.mu.Lock(); d.exposing = b; d.mu.Unlock() }
func (d *Device) isExposing() bool    { d.mu.Lock(); defer d.mu.Unlock(); return d.exposing }
func (d *Device) exposeCtx() context.Context {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ctx != nil {
		return d.ctx
	}
	return context.Background()
}

// encodeFITS wraps a raw mono frame in a minimal FITS file PHD2 reads directly.
// bits selects the depth: 16-bit input is little-endian uint16 → FITS BITPIX 16 with
// BZERO 32768 (the standard unsigned-16 carrier); 8-bit input is bytes → BITPIX 8.
// The pixel data is otherwise untouched (raw, no transformation).
func encodeFITS(w, h, bits int, pixels []byte) []byte {
	bitpix, bzero := 16, 32768
	if bits <= 8 {
		bitpix, bzero = 8, 0
	}
	var hdr []byte
	card := func(s string) {
		if len(s) > 80 {
			s = s[:80]
		}
		hdr = append(hdr, s...)
		for i := len(s); i < 80; i++ {
			hdr = append(hdr, ' ')
		}
	}
	card(fmt.Sprintf("%-8s= %20s", "SIMPLE", "T"))
	card(fmt.Sprintf("%-8s= %20d", "BITPIX", bitpix))
	card(fmt.Sprintf("%-8s= %20d", "NAXIS", 2))
	card(fmt.Sprintf("%-8s= %20d", "NAXIS1", w))
	card(fmt.Sprintf("%-8s= %20d", "NAXIS2", h))
	card(fmt.Sprintf("%-8s= %20d", "BZERO", bzero))
	card(fmt.Sprintf("%-8s= %20d", "BSCALE", 1))
	card("END")
	for len(hdr)%2880 != 0 {
		hdr = append(hdr, ' ')
	}

	n := w * h
	var data []byte
	if bitpix == 8 {
		data = make([]byte, n)
		copy(data, pixels) // bytes as-is (unsigned 0..255)
	} else {
		data = make([]byte, n*2)
		for i := 0; i < n && (i*2+1) < len(pixels); i++ {
			u := binary.LittleEndian.Uint16(pixels[i*2:])
			binary.BigEndian.PutUint16(data[i*2:], uint16(int16(int(u)-32768)))
		}
	}
	for len(data)%2880 != 0 {
		data = append(data, 0)
	}
	return append(hdr, data...)
}

var (
	_ server.Device  = (*Device)(nil)
	_ server.Starter = (*Device)(nil)
)

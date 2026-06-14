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
	controls   *server.Property // CCD_CONTROLS (gain/offset) — defined on connect if supported
	frame      *server.Property // CCD_FRAME (subframe ROI) — defined on connect if supported

	mu         sync.Mutex
	connected  bool
	exposing   bool
	ctx        context.Context    // server context (set in Start); exposures derive from it
	cancelExp  context.CancelFunc // cancels the in-flight exposure's awaitFrame goroutine
	exposeDone chan struct{}      // closed when the in-flight awaitFrame goroutine exits
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
	props := []*server.Property{d.conn, d.driverInfo, d.info, d.exposure, d.abort, d.binning, d.blob}
	// CCD_CONTROLS / CCD_FRAME are advertised only once a capable camera has connected
	// (see defineCaps); include them so clients connecting afterward enumerate them too.
	d.mu.Lock()
	if d.controls != nil {
		props = append(props, d.controls)
	}
	if d.frame != nil {
		props = append(props, d.frame)
	}
	d.mu.Unlock()
	return props
}

// defineCaps advertises the optional CCD_CONTROLS (gain/offset) and CCD_FRAME (subframe)
// properties the moment a camera that supports them connects, and refreshes their values
// on every (re)connect. Real INDI camera drivers define these device-specific properties
// post-connect; clients pick them up via newProperty.
func (d *Device) defineCaps(pub server.Publisher, c Camera) {
	var define, update []*server.Property

	if gc, ok := c.(GainController); ok {
		gv, gmin, gmax := gc.Gain()
		ov, omin, omax, hasOff := 0, 0, 0, false
		if oc, ok := c.(OffsetController); ok {
			ov, omin, omax = oc.Offset()
			hasOff = true
		}
		d.mu.Lock()
		if d.controls == nil {
			d.controls = controlsProperty(d.name, gv, gmin, gmax, ov, omin, omax, hasOff)
			define = append(define, d.controls)
		} else {
			d.controls.SetNumber("Gain", float64(gv))
			if hasOff {
				d.controls.SetNumber("Offset", float64(ov))
			}
			update = append(update, d.controls)
		}
		d.mu.Unlock()
	}

	if sf, ok := c.(Subframer); ok {
		x, y, w, h := sf.Subframe()
		mw, mh := c.Size()
		d.mu.Lock()
		if d.frame == nil {
			d.frame = frameProperty(d.name, x, y, w, h, mw, mh)
			define = append(define, d.frame)
		} else {
			d.frame.SetNumber("X", float64(x))
			d.frame.SetNumber("Y", float64(y))
			d.frame.SetNumber("WIDTH", float64(w))
			d.frame.SetNumber("HEIGHT", float64(h))
			update = append(update, d.frame)
		}
		d.mu.Unlock()
	}

	for _, p := range define {
		pub.Define(p)
	}
	for _, p := range update {
		pub.Update(p)
	}
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
				c, _ := d.cam()
				d.stopExposure(c)
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
	case "CCD_CONTROLS":
		c, err := d.cam()
		if err != nil {
			return
		}
		for _, m := range members {
			switch m.Name {
			case "Gain":
				if gc, ok := c.(GainController); ok {
					_ = gc.SetGain(int(m.Float()))
				}
			case "Offset":
				if oc, ok := c.(OffsetController); ok {
					_ = oc.SetOffset(int(m.Float()))
				}
			}
		}
		d.mu.Lock()
		p := d.controls
		d.mu.Unlock()
		if p != nil { // report back the values the camera actually accepted
			if gc, ok := c.(GainController); ok {
				gv, _, _ := gc.Gain()
				p.SetNumber("Gain", float64(gv))
			}
			if oc, ok := c.(OffsetController); ok {
				ov, _, _ := oc.Offset()
				p.SetNumber("Offset", float64(ov))
			}
			p.SetState(server.Ok)
			pub.Update(p)
		}
	case "CCD_FRAME":
		c, err := d.cam()
		if err != nil {
			return
		}
		sf, ok := c.(Subframer)
		if !ok {
			return
		}
		x, y, w, h := sf.Subframe()
		for _, m := range members {
			switch m.Name {
			case "X":
				x = int(m.Float())
			case "Y":
				y = int(m.Float())
			case "WIDTH":
				w = int(m.Float())
			case "HEIGHT":
				h = int(m.Float())
			}
		}
		_ = sf.SetSubframe(x, y, w, h)
		rx, ry, rw, rh := sf.Subframe()
		d.mu.Lock()
		p := d.frame
		d.mu.Unlock()
		if p != nil {
			p.SetNumber("X", float64(rx))
			p.SetNumber("Y", float64(ry))
			p.SetNumber("WIDTH", float64(rw))
			p.SetNumber("HEIGHT", float64(rh))
			p.SetState(server.Ok)
			pub.Update(p)
		}
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
		d.defineCaps(pub, c)
	} else {
		c, _ := d.cam()
		d.stopExposure(c) // leave the camera idle for the next client
		d.setConnected(false)
		d.conn.SetSwitch("DISCONNECT", true)
		d.exposure.SetState(server.Idle)
		pub.Update(d.exposure)
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
// pushes it as a BLOB. A new exposure supersedes any in-flight one — stopExposure
// cancels the prior awaitFrame goroutine and aborts the camera so StartExposure
// always begins from a clean state. Without that, a client that re-exposes after a
// timeout (PHD2's reconnect loop) stacks overlapping exposures on the shared camera
// and wedges it until the fleet is restarted.
func (d *Device) startExposure(pub server.Publisher, secs float64) {
	c, err := d.cam()
	if err != nil {
		d.exposure.SetState(server.Alert)
		pub.Update(d.exposure)
		return
	}
	d.stopExposure(c)
	if err := c.StartExposure(secs); err != nil {
		d.exposure.SetState(server.Alert)
		pub.Update(d.exposure)
		pub.Message(d.name, "expose: "+err.Error())
		return
	}
	ctx, cancel := context.WithCancel(d.serverCtx())
	done := make(chan struct{})
	d.mu.Lock()
	d.exposing = true
	d.cancelExp = cancel
	d.exposeDone = done
	d.mu.Unlock()
	d.exposure.SetNumber("CCD_EXPOSURE_VALUE", secs)
	d.exposure.SetState(server.Busy)
	pub.Update(d.exposure)
	go d.awaitFrame(ctx, done, pub, c)
}

func (d *Device) awaitFrame(ctx context.Context, done chan struct{}, pub server.Publisher, c Camera) {
	defer close(done)
	tick := time.NewTicker(d.poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return // superseded, aborted, disconnected, or shutting down
		case <-tick.C:
			if !c.ImageReady() {
				continue
			}
			w, h, px, err := c.Frame()
			if err != nil {
				d.clearExposing()
				d.exposure.SetState(server.Alert)
				pub.Update(d.exposure)
				pub.Message(d.name, "frame: "+err.Error())
				return
			}
			// RAW frame straight to the BLOB — no transformation.
			pub.SendBLOB(d.name, "CCD1", "CCD1", ".fits", encodeFITS(w, h, c.BitsPerPixel(), px))
			d.clearExposing()
			d.exposure.SetNumber("CCD_EXPOSURE_VALUE", 0)
			d.exposure.SetState(server.Ok)
			pub.Update(d.exposure)
			return
		}
	}
}

// stopExposure cancels any in-flight exposure goroutine, waits for it to exit, and
// aborts the camera so the next StartExposure starts clean. Safe to call when nothing
// is exposing. The bounded wait keeps a blocked USB read from wedging the read loop.
func (d *Device) stopExposure(c Camera) {
	d.mu.Lock()
	cancel, done, was := d.cancelExp, d.exposeDone, d.exposing
	d.cancelExp, d.exposeDone, d.exposing = nil, nil, false
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
	if was && c != nil {
		_ = c.AbortExposure()
	}
}

// Start records the server context so an in-flight exposure goroutine stops on shutdown.
func (d *Device) Start(ctx context.Context, _ server.Publisher) {
	d.mu.Lock()
	d.ctx = ctx
	d.mu.Unlock()
}

func (d *Device) setConnected(b bool) { d.mu.Lock(); d.connected = b; d.mu.Unlock() }

// clearExposing marks the exposure finished when its goroutine completes on its own.
func (d *Device) clearExposing() {
	d.mu.Lock()
	d.exposing, d.cancelExp, d.exposeDone = false, nil, nil
	d.mu.Unlock()
}

// serverCtx is the server-lifetime context (set in Start); per-exposure contexts derive
// from it so a server shutdown also cancels any in-flight exposure.
func (d *Device) serverCtx() context.Context {
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

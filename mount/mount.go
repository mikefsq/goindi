// Package mount is a generic INDI telescope+guider device over any lx200.Mount.
package mount

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/mikefsq/goindi/server"
	"github.com/mikefsq/lx200"
)

// MountFunc returns the mount that is live right now, or an error if it is not connected.
type MountFunc func() (lx200.Mount, error)

// Optics supplies the optical-train parameters for TELESCOPE_INFO, in millimetres.
type Optics interface {
	OpticsMM() (aperture, focalLength, guiderAperture, guiderFocalLength float64)
}

// Option configures a Device.
type Option func(*Device)

// WithOptics makes the device report TELESCOPE_INFO from o; without it there is no
// TELESCOPE_INFO.
func WithOptics(o Optics) Option { return func(d *Device) { d.optics = o } }

// Device is the INDI mount adapter; build it with New and register it with a server.Server.
type Device struct {
	name  string
	mount MountFunc
	poll  time.Duration

	conn       *server.Property
	driverInfo *server.Property
	eq         *server.Property
	onSet      *server.Property
	abort      *server.Property
	pier       *server.Property
	guideNS    *server.Property
	guideWE    *server.Property
	guideRate  *server.Property // GUIDE_RATE, fraction of sidereal
	info       *server.Property // TELESCOPE_INFO, present only when optics is set
	dualAxis   *server.Property // DUAL_AXIS_TRACKING, defined on connect for DualAxisTracker mounts

	optics       Optics
	lastOptics   [4]float64 // last published, so TELESCOPE_INFO only goes out on change
	guideRateVal float64

	mu          sync.Mutex
	connected   bool
	dualAxisCap bool
	ctx         context.Context // set by Start; bounds the guide-completion timer
}

var errReject = errors.New("mount rejected target")

// New builds the device named name (the INDI device id clients pick) over m.
func New(name string, m MountFunc, opts ...Option) *Device {
	d := &Device{name: name, mount: m, poll: time.Second, guideRateVal: 0.5}
	d.conn = server.ConnectionProperty(name)
	d.driverInfo = server.DriverInfoProperty(name, name, "goindi-mount", "1.0",
		server.InterfaceTelescope|server.InterfaceGuider)
	d.eq = server.EquatorialCoordProperty(name)
	d.onSet = server.OnCoordSetProperty(name)
	d.abort = server.AbortProperty(name)
	d.pier = server.PierSideProperty(name)
	d.guideNS = server.TimedGuideNSProperty(name)
	d.guideWE = server.TimedGuideWEProperty(name)
	for _, o := range opts {
		o(d)
	}
	d.guideRate = server.GuideRateProperty(name, d.guideRateVal)
	d.dualAxis = server.DualAxisTrackingProperty(name) // built here, defined only if the mount supports it
	if d.optics != nil {
		d.info = server.TelescopeInfoProperty(name)
		d.refreshOptics() // seed the members so the initial def carries real values
	}
	return d
}

// WithGuideRate sets the reported guide rate as a fraction of sidereal (e.g. 0.5).
func WithGuideRate(rate float64) Option {
	return func(d *Device) {
		if rate > 0 {
			d.guideRateVal = rate
		}
	}
}

func (d *Device) Name() string { return d.name }

func (d *Device) Properties() []*server.Property {
	props := []*server.Property{d.conn, d.driverInfo, d.eq, d.onSet, d.abort, d.pier, d.guideNS, d.guideWE, d.guideRate}
	if d.info != nil {
		props = append(props, d.info)
	}
	d.mu.Lock()
	advertise := d.dualAxisCap
	d.mu.Unlock()
	if advertise { // include it so a client connecting after refreshDualAxis still sees it
		props = append(props, d.dualAxis)
	}
	return props
}

// refreshOptics copies the current optics into TELESCOPE_INFO, reporting whether they changed.
func (d *Device) refreshOptics() bool {
	ap, fl, gap, gfl := d.optics.OpticsMM()
	now := [4]float64{ap, fl, gap, gfl}
	d.mu.Lock()
	changed := now != d.lastOptics
	d.lastOptics = now
	d.mu.Unlock()
	d.info.SetNumber("TELESCOPE_APERTURE", ap)
	d.info.SetNumber("TELESCOPE_FOCAL_LENGTH", fl)
	d.info.SetNumber("GUIDER_APERTURE", gap)
	d.info.SetNumber("GUIDER_FOCAL_LENGTH", gfl)
	return changed
}

func (d *Device) HandleNew(pub server.Publisher, name string, members []server.NewMember) {
	switch name {
	case "CONNECTION":
		d.handleConnection(pub, members)
	case "ON_COORD_SET":
		for _, m := range members {
			if m.On() {
				d.onSet.SetSwitch(m.Name, true)
			}
		}
		d.onSet.SetState(server.Ok)
		pub.Update(d.onSet)
	case "EQUATORIAL_EOD_COORD":
		d.handleEq(pub, members)
	case "TELESCOPE_ABORT_MOTION":
		d.handleAbort(pub, members)
	case "TELESCOPE_TIMED_GUIDE_NS":
		d.handleGuide(pub, d.guideNS, members)
	case "TELESCOPE_TIMED_GUIDE_WE":
		d.handleGuide(pub, d.guideWE, members)
	case "GUIDE_RATE":
		// Reported only: LX200 has no command to set the guide rate.
		for _, m := range members {
			f, ok := m.Float()
			if !ok {
				d.guideRate.SetState(server.Alert)
				pub.Update(d.guideRate)
				pub.Message(d.name, fmt.Sprintf("guide rate: bad number %q", m.Value))
				return
			}
			d.guideRate.SetNumber(m.Name, f)
		}
		d.guideRate.SetState(server.Ok)
		pub.Update(d.guideRate)
	case "DUAL_AXIS_TRACKING":
		d.handleDualAxis(pub, members)
	}
}

// refreshGuideRate replaces the configured rate with the mount's real one, when it can
// report it. A mount that can't keeps the configured default.
func (d *Device) refreshGuideRate(pub server.Publisher, m lx200.Mount) {
	gr, ok := m.(lx200.GuideRater)
	if !ok {
		return
	}
	rate, err := gr.GuideRateSidereal()
	if err != nil || rate <= 0 {
		return
	}
	d.guideRate.SetNumber("GUIDE_RATE_WE", rate)
	d.guideRate.SetNumber("GUIDE_RATE_NS", rate)
	d.guideRate.SetState(server.Ok)
	pub.Update(d.guideRate)
}

// refreshDualAxis defines DUAL_AXIS_TRACKING, seeded from the mount, if the mount supports it.
func (d *Device) refreshDualAxis(pub server.Publisher, m lx200.Mount) {
	dt, ok := m.(lx200.DualAxisTracker)
	if !ok {
		return
	}
	// Capability is the type assertion, not the state read: a transient read error
	// must not hide the control for the whole session.
	if on, err := dt.DualAxisTracking(); err == nil {
		d.setDualAxisSwitch(on)
	}
	d.mu.Lock()
	d.dualAxisCap = true
	d.mu.Unlock()
	pub.Define(d.dualAxis)
}

func (d *Device) clearDualAxis(pub server.Publisher) {
	d.mu.Lock()
	had := d.dualAxisCap
	d.dualAxisCap = false
	d.mu.Unlock()
	if had {
		pub.Delete(d.name, "DUAL_AXIS_TRACKING")
	}
}

// One SetSwitch suffices: OneOfMany turns the sibling member Off automatically.
func (d *Device) setDualAxisSwitch(on bool) {
	member := "DISABLE"
	if on {
		member = "ENABLE"
	}
	d.dualAxis.SetSwitch(member, true)
	d.dualAxis.SetState(server.Ok)
}

// A rejected set is expected: mounts refuse to disable dual-axis tracking outside
// equatorial mode.
func (d *Device) handleDualAxis(pub server.Publisher, members []server.NewMember) {
	alert := func(msg string) {
		d.dualAxis.SetState(server.Alert)
		pub.Update(d.dualAxis)
		if msg != "" {
			pub.Message(d.name, "dual-axis tracking: "+msg)
		}
	}
	m, err := d.mount()
	if err != nil {
		alert("")
		return
	}
	dt, ok := m.(lx200.DualAxisTracker)
	if !ok {
		alert("")
		return
	}
	enable, got := selectedOn(members, "ENABLE", "DISABLE")
	if !got {
		return
	}
	if err := dt.SetDualAxisTracking(enable); err != nil {
		alert(err.Error())
		return
	}
	d.setDualAxisSwitch(enable)
	pub.Update(d.dualAxis)
}

// Only an On counts, so a lone "Off" is a no-op rather than an asymmetric command.
func selectedOn(members []server.NewMember, onName, offName string) (on, ok bool) {
	for _, m := range members {
		if !m.On() {
			continue
		}
		switch m.Name {
		case onName:
			return true, true
		case offName:
			return false, true
		}
	}
	return false, false
}

func (d *Device) handleConnection(pub server.Publisher, members []server.NewMember) {
	connect, _ := selectedOn(members, "CONNECT", "DISCONNECT")
	if connect {
		m, err := d.mount()
		if err != nil {
			d.conn.SetState(server.Alert)
			pub.Update(d.conn)
			pub.Message(d.name, "connect failed: "+err.Error())
			return
		}
		d.setConnected(true)
		d.conn.SetSwitch("CONNECT", true)
		d.refreshGuideRate(pub, m)
		d.refreshDualAxis(pub, m)
	} else {
		d.setConnected(false)
		d.conn.SetSwitch("DISCONNECT", true)
		d.clearDualAxis(pub)
	}
	d.conn.SetState(server.Ok)
	pub.Update(d.conn)
}

func (d *Device) handleEq(pub server.Publisher, members []server.NewMember) {
	m, err := d.mount()
	if err != nil {
		d.eq.SetState(server.Alert)
		pub.Update(d.eq)
		return
	}
	var ra, dec float64
	haveRA, haveDec := false, false
	for _, mm := range members {
		if mm.Name != "RA" && mm.Name != "DEC" {
			continue
		}
		v, ok := mm.Float()
		if !ok {
			// Reject the whole command: a malformed coordinate must not slew, least of all to 0.
			d.eq.SetState(server.Alert)
			pub.Update(d.eq)
			pub.Message(d.name, fmt.Sprintf("goto: bad %s value %q", mm.Name, mm.Value))
			return
		}
		if mm.Name == "RA" {
			ra, haveRA = v, true
		} else {
			dec, haveDec = v, true
		}
	}
	if !haveRA || !haveDec {
		return
	}
	sync := d.onSet.Switch("SYNC")
	d.eq.SetState(server.Busy)
	pub.Update(d.eq)
	// Off the read loop: a goto can take seconds.
	go func() {
		err := withOp(m, func() error {
			if ok, e := m.SetTargetRA(ra); e != nil {
				return e
			} else if !ok {
				return errReject
			}
			if ok, e := m.SetTargetDec(dec); e != nil {
				return e
			} else if !ok {
				return errReject
			}
			if sync {
				_, e := m.SyncToTarget()
				return e
			}
			return m.SlewToTarget()
		})
		switch {
		case err != nil:
			d.eq.SetState(server.Alert)
			pub.Message(d.name, "goto: "+err.Error())
		case sync:
			d.eq.SetState(server.Ok)
		default:
			// Slew in progress; the poll loop flips Busy→Ok on completion.
		}
		pub.Update(d.eq)
	}()
}

func (d *Device) handleAbort(pub server.Publisher, members []server.NewMember) {
	for _, mm := range members {
		if mm.Name == "ABORT" && mm.On() {
			if m, err := d.mount(); err == nil {
				_ = m.Halt()
			}
		}
	}
	d.abort.SetSwitch("ABORT", false)
	d.abort.SetState(server.Ok)
	pub.Update(d.abort)
}

// handleGuide issues the pulse straight to the mount, which then guides autonomously
// for the duration.
func (d *Device) handleGuide(pub server.Publisher, prop *server.Property, members []server.NewMember) {
	m, err := d.mount()
	if err != nil {
		prop.SetState(server.Alert)
		pub.Update(prop)
		return
	}
	g, ok := m.(lx200.Guider)
	if !ok {
		prop.SetState(server.Alert)
		pub.Update(prop)
		pub.Message(d.name, "mount does not support pulse guiding")
		return
	}
	maxMs := 0
	for _, mm := range members {
		f, ok := mm.Float()
		if !ok {
			// A malformed duration must not pulse at all.
			prop.SetState(server.Alert)
			pub.Update(prop)
			pub.Message(d.name, fmt.Sprintf("pulse guide: bad duration %q", mm.Value))
			return
		}
		ms := int(f)
		if ms <= 0 {
			continue
		}
		dir, ok := guideDir(mm.Name)
		if !ok {
			continue
		}
		if err := g.PulseGuide(dir, ms); err != nil { // :Mg# returns at once, it does not block for ms
			pub.Message(d.name, "pulse guide: "+err.Error())
			continue
		}
		prop.SetNumber(mm.Name, float64(ms))
		if ms > maxMs {
			maxMs = ms
		}
	}
	if maxMs == 0 {
		prop.SetState(server.Ok)
		pub.Update(prop)
		return
	}
	// Hold Busy for the duration: reporting "done" while the mount is still moving would
	// let a waiting client measure mid-correction.
	prop.SetState(server.Busy)
	pub.Update(prop)
	go func(ms int) {
		timer := time.NewTimer(time.Duration(ms) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-d.guideCtx().Done():
			return
		case <-timer.C:
		}
		for _, name := range []string{"TIMED_GUIDE_N", "TIMED_GUIDE_S", "TIMED_GUIDE_W", "TIMED_GUIDE_E"} {
			prop.SetNumber(name, 0)
		}
		prop.SetState(server.Ok)
		pub.Update(prop)
	}(maxMs)
}

// Background until Start has run, so a pulse issued in the startup window still completes.
func (d *Device) guideCtx() context.Context {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ctx != nil {
		return d.ctx
	}
	return context.Background()
}

func guideDir(member string) (lx200.Direction, bool) {
	switch member {
	case "TIMED_GUIDE_N":
		return lx200.North, true
	case "TIMED_GUIDE_S":
		return lx200.South, true
	case "TIMED_GUIDE_W":
		return lx200.West, true
	case "TIMED_GUIDE_E":
		return lx200.East, true
	}
	return 0, false
}

// Start polls the connected mount and publishes its live position and pier side.
func (d *Device) Start(ctx context.Context, pub server.Publisher) {
	d.mu.Lock()
	d.ctx = ctx
	d.mu.Unlock()
	t := time.NewTicker(d.poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Optics is config, so it is published whether or not the mount is connected.
			if d.optics != nil && d.refreshOptics() {
				pub.Update(d.info)
			}
			if !d.isConnected() {
				continue
			}
			m, err := d.mount()
			if err != nil {
				continue
			}
			ra, e1 := m.RA()
			dec, e2 := m.Dec()
			if e1 != nil || e2 != nil {
				continue
			}
			d.eq.SetNumber("RA", ra)
			d.eq.SetNumber("DEC", dec)
			if slewing, err := m.Slewing(); err == nil && slewing {
				d.eq.SetState(server.Busy)
			} else {
				d.eq.SetState(server.Ok)
			}
			pub.Update(d.eq)

			if ps, ok := m.(lx200.PierSider); ok {
				if side, err := ps.PierSide(); err == nil && side != lx200.PierUnknown {
					d.pier.SetSwitch("PIER_WEST", side == lx200.PierWest)
					d.pier.SetSwitch("PIER_EAST", side == lx200.PierEast)
					d.pier.SetState(server.Ok)
					pub.Update(d.pier)
				}
			}
		}
	}
}

func (d *Device) setConnected(b bool) { d.mu.Lock(); d.connected = b; d.mu.Unlock() }
func (d *Device) isConnected() bool   { d.mu.Lock(); defer d.mu.Unlock(); return d.connected }

// withOp runs f under the mount's OpLock, keeping set-target-then-act atomic against
// other front-ends sharing this mount.
func withOp(m lx200.Mount, f func() error) error {
	if l, ok := m.(lx200.OpLocker); ok {
		defer l.OpLock()()
	}
	return f()
}

var _ server.Device = (*Device)(nil)
var _ server.Starter = (*Device)(nil)

// Package mount is a generic INDI telescope+guider device over any lx200.Mount.
// Because every fleet mount (tenmicron, am5, rst, onstep) satisfies lx200.Mount,
// this single adapter serves them all — the same way the LX200 bridge does — and
// it is a sibling front-end onto the same source-of-truth mount, sharing its
// OpLock so an INDI-driven slew and an Alpaca-driven slew cannot corrupt the
// device's target register.
package mount

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/mikefsq/goindi/server"
	"github.com/mikefsq/lx200"
)

// MountFunc returns the mount that is live right now, or an error if it is not
// connected. The device calls it per operation, so a reconnect on the owning side
// is transparent and no stale handle or device state is cached.
type MountFunc func() (lx200.Mount, error)

// Optics supplies the optical-train parameters for TELESCOPE_INFO, in millimetres
// (aperture, focal length, then the guide scope's aperture and focal length). The
// mount can't measure these; they come from the driver's optics config. It is read
// live on each poll, so a runtime change — an Alpaca setoptics Action on the shared
// holder — is reflected here without restart. Zero values are reported as 0.
type Optics interface {
	OpticsMM() (aperture, focalLength, guiderAperture, guiderFocalLength float64)
}

// Option configures a Device.
type Option func(*Device)

// WithOptics makes the device report TELESCOPE_INFO from o (the optical-train
// parameters). Without it, the device exposes no TELESCOPE_INFO.
func WithOptics(o Optics) Option { return func(d *Device) { d.optics = o } }

// Device is the INDI adapter. Build it with New and register it with a server.Server.
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
	guideRate  *server.Property // GUIDE_RATE (fraction of sidereal) — PHD2 reads this
	info       *server.Property // TELESCOPE_INFO, present only when optics is set
	dualAxis   *server.Property // DUAL_AXIS_TRACKING, defined on connect for DualAxisTracker mounts

	optics       Optics
	lastOptics   [4]float64 // last-published TELESCOPE_INFO values, to publish only on change
	guideRateVal float64    // reported guide rate (default 0.5x sidereal)

	mu          sync.Mutex
	connected   bool
	dualAxisCap bool            // connected mount supports dual-axis tracking (lx200.DualAxisTracker)
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
	d.dualAxis = server.DualAxisTrackingProperty(name) // defined on connect only if supported
	if d.optics != nil {
		d.info = server.TelescopeInfoProperty(name)
		d.refreshOptics() // seed the members from the holder for the initial def
	}
	return d
}

// WithGuideRate sets the reported guide rate (fraction of sidereal, e.g. 0.5). PHD2
// reads it to scale calibration; set it to match the mount's actual guide-speed setting.
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
	if advertise { // only advertised while a capable mount is connected (so late clients see it)
		props = append(props, d.dualAxis)
	}
	return props
}

// refreshOptics copies the holder's current values into TELESCOPE_INFO and reports
// whether they changed since the last publish. Caller publishes on change.
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
		// Store the client-set rate. Most lx200 mounts have no settable guide-rate
		// command through this interface, so it is not pushed to hardware here.
		for _, m := range members {
			d.guideRate.SetNumber(m.Name, m.Float())
		}
		d.guideRate.SetState(server.Ok)
		pub.Update(d.guideRate)
	case "DUAL_AXIS_TRACKING":
		d.handleDualAxis(pub, members)
	}
}

// refreshGuideRate reads the mount's actual guide rate (fraction of sidereal) when it
// can report one (lx200.GuideRater — tenmicron, am5), overriding the configured
// default so PHD2 sees the true value. Mounts that can't report it (rst, onstep, the
// sim) keep the configured default.
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

// refreshDualAxis exposes the DUAL_AXIS_TRACKING switch for mounts that support it
// (lx200.DualAxisTracker — 10Micron drives both axes to follow its refraction/pointing
// model), seeding it with the mount's current state and defining the property. Mounts
// without the capability (am5, rst, onstep, sim) never see it. Paired with clearDualAxis
// on disconnect.
func (d *Device) refreshDualAxis(pub server.Publisher, m lx200.Mount) {
	dt, ok := m.(lx200.DualAxisTracker)
	if !ok {
		return
	}
	// The capability is the type assertion, not the state read: a transient error
	// on the initial query must not hide the control for the whole session. Seed the
	// switch when we can read it, otherwise define it at its default and let the
	// client's first set correct it.
	if on, err := dt.DualAxisTracking(); err == nil {
		d.setDualAxisSwitch(on)
	}
	d.mu.Lock()
	d.dualAxisCap = true
	d.mu.Unlock()
	pub.Define(d.dualAxis)
}

// clearDualAxis removes the DUAL_AXIS_TRACKING property on disconnect (if it was defined).
func (d *Device) clearDualAxis(pub server.Publisher) {
	d.mu.Lock()
	had := d.dualAxisCap
	d.dualAxisCap = false
	d.mu.Unlock()
	if had {
		pub.Delete(d.name, "DUAL_AXIS_TRACKING")
	}
}

// setDualAxisSwitch reflects the on/off state in the OneOfMany ENABLE/DISABLE members.
// One SetSwitch suffices: OneOfMany turns the sibling member Off automatically.
func (d *Device) setDualAxisSwitch(on bool) {
	member := "DISABLE"
	if on {
		member = "ENABLE"
	}
	d.dualAxis.SetSwitch(member, true)
	d.dualAxis.SetState(server.Ok)
}

// handleDualAxis applies a DUAL_AXIS_TRACKING set (ENABLE/DISABLE) to the mount. An
// unsupported mount or a rejected set (disabling is equatorial-only) reports Alert.
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

// selectedOn resolves a two-member OneOfMany switch set to the boolean the client
// asked for: (true, true) when the positive member is turned On, (false, true) when
// the negative is, and (_, false) when neither member is On. It reacts only to the
// member switched On — the same way a radio group is driven — so a lone "Off" on
// either member is a no-op rather than an asymmetric command.
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
		d.refreshGuideRate(pub, m) // report the mount's real guide rate if it has one
		d.refreshDualAxis(pub, m)  // expose the dual-axis switch if the mount supports it
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
		switch mm.Name {
		case "RA":
			ra, haveRA = mm.Float(), true
		case "DEC":
			dec, haveDec = mm.Float(), true
		}
	}
	if !haveRA || !haveDec {
		return
	}
	sync := d.onSet.Switch("SYNC")
	d.eq.SetState(server.Busy)
	pub.Update(d.eq)
	// Run the (possibly slow) goto off the read loop. The OpLock keeps the whole
	// set-target-then-act sequence atomic against the Alpaca front-end.
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
			// A slew is now in progress; the poll loop flips Busy→Ok on completion.
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

// handleGuide is the latency-critical path PHD2's control loop is tuned around, so
// it stays as close to driver-to-driver as the protocol allows: parse the
// new-vector, issue the pulse straight to the mount (lx200 :Mg#) with no buffering
// or transformation. The mount then guides autonomously for the pulse duration.
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
		ms := int(mm.Float())
		if ms <= 0 {
			continue
		}
		dir, ok := guideDir(mm.Name)
		if !ok {
			continue
		}
		if err := g.PulseGuide(dir, ms); err != nil { // immediate; :Mg# returns at once
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
	// Hold the property Busy for exactly the pulse duration, then report completion.
	// The mount is already moving (the :Mg# above fired immediately); this only makes
	// the completion signal honest so a client that waits on the property — the INDI
	// contract — sees the true pulse time instead of an instant "done" that would let
	// it measure mid-correction and destabilize the loop. It adds no latency (the
	// client waits the duration regardless) and does not block the read loop.
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

// guideCtx is the context bounding the guide-completion timer (Background until
// Start has run, so a pulse issued in the startup window still completes).
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

// Start polls the mount once connected and publishes live position + pier side, so
// clients (PHD2's Dec compensation, the chart reticle) track the real mount.
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
			// Optics is config, available whether or not the mount is connected;
			// republish TELESCOPE_INFO only when it changes (e.g. a setoptics Action).
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

// withOp runs f under the mount's OpLock if it provides one, serializing the
// set-target-then-act sequence against other front-ends (the Alpaca wrapper, the
// LX200 bridge) sharing this mount.
func withOp(m lx200.Mount, f func() error) error {
	if l, ok := m.(lx200.OpLocker); ok {
		defer l.OpLock()()
	}
	return f()
}

var _ server.Device = (*Device)(nil)
var _ server.Starter = (*Device)(nil)

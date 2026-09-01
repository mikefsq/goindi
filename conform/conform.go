// Package conform is a black-box validator that drives any INDI server and reports
// whether it conforms to the protocol and the standard property contracts.
package conform

import (
	"bytes"
	"compress/zlib"
	"context"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mikefsq/goindi/client"
)

// Status is a check outcome.
type Status int

const (
	Pass Status = iota
	Fail
	Warn
	Info
)

// String is the four-letter status name used in reports.
func (s Status) String() string {
	switch s {
	case Fail:
		return "FAIL"
	case Warn:
		return "WARN"
	case Info:
		return "INFO"
	default:
		return "PASS"
	}
}

// Result is one named check outcome.
type Result struct {
	Device string
	Check  string
	Status Status
	Detail string
}

// Options tunes a conformance run.
type Options struct {
	Device  string        // limit to one device ("" = all discovered)
	Mutate  bool          // run state-changing checks (connect, guide pulse, test exposure)
	Timeout time.Duration // per-wait timeout (default 3s)
}

// DRIVER_INTERFACE bits.
const (
	ifTelescope    = 1 << 0
	ifCCD          = 1 << 1
	ifGuider       = 1 << 2
	ifFocuser      = 1 << 3
	ifFilter       = 1 << 4
	ifDome         = 1 << 5
	ifGPS          = 1 << 6
	ifWeather      = 1 << 7
	ifAO           = 1 << 8
	ifDustcap      = 1 << 9
	ifLightbox     = 1 << 10
	ifDetector     = 1 << 11
	ifRotator      = 1 << 12
	ifSpectrograph = 1 << 13
	ifCorrelator   = 1 << 14
	ifAux          = 1 << 15
)

// contractBits are the interfaces with property-contract checks; the rest get their
// names decoded but no per-property requirements.
const contractBits = ifTelescope | ifCCD | ifGuider | ifFocuser | ifFilter |
	ifDome | ifGPS | ifWeather | ifRotator

type report struct {
	dev     string
	results []Result
}

func (r *report) add(check string, st Status, detail string) {
	r.results = append(r.results, Result{Device: r.dev, Check: check, Status: st, Detail: detail})
}
func (r *report) pass(check string)         { r.add(check, Pass, "") }
func (r *report) fail(check, detail string) { r.add(check, Fail, detail) }
func (r *report) warn(check, detail string) { r.add(check, Warn, detail) }
func (r *report) info(check, detail string) { r.add(check, Info, detail) }
func (r *report) want(check string, ok bool, detail string) {
	if ok {
		r.pass(check)
	} else {
		r.fail(check, detail)
	}
}

var (
	validState = map[string]bool{"Idle": true, "Ok": true, "Busy": true, "Alert": true}
	validPerm  = map[string]bool{"ro": true, "wo": true, "rw": true}
	validRule  = map[string]bool{"OneOfMany": true, "AtMostOne": true, "AnyOfMany": true}
)

// Run executes the conformance battery against c and returns the results.
func Run(ctx context.Context, c *client.Client, opts Options) []Result {
	if opts.Timeout == 0 {
		opts.Timeout = 3 * time.Second
	}
	r := &report{}

	_ = c.GetProperties("", "")
	if opts.Device != "" {
		// Wait for this device by name: an absent one must FAIL, not pass zero checks.
		if !waitForDevice(ctx, c, opts.Device, waitBudget(ctx, opts.Timeout)) {
			if ctx.Err() != nil {
				r.fail("run", "aborted: "+ctx.Err().Error())
			} else {
				r.fail("device not found",
					fmt.Sprintf("device %q not defined within timeout (devices seen: %v)", opts.Device, c.Devices()))
			}
			return r.results
		}
	} else if !c.WaitDevices(1, waitBudget(ctx, opts.Timeout)) {
		if ctx.Err() != nil {
			r.fail("run", "aborted: "+ctx.Err().Error())
		} else {
			r.fail("discovery", "no devices defined after getProperties")
		}
		return r.results
	}
	// The first def opens the burst rather than closing it; more properties and more
	// devices stream in behind it.
	settle(ctx, opts.Timeout, func() int {
		n := len(c.Devices())
		for _, d := range c.Devices() {
			n += len(c.Properties(d))
		}
		return n
	})
	devs := c.Devices()
	r.info("discovery", fmt.Sprintf("%d device(s): %v", len(devs), devs))

	for _, dev := range devs {
		if opts.Device != "" && dev != opts.Device {
			continue
		}
		if ctx.Err() != nil {
			r.dev = ""
			r.fail("run", "aborted: "+ctx.Err().Error())
			return r.results
		}
		r.dev = dev
		checkDevice(ctx, c, dev, opts, r)
	}
	r.dev = ""
	if ctx.Err() != nil {
		r.fail("run", "aborted: "+ctx.Err().Error())
	}
	return r.results
}

// waitBudget clamps d to the time left before ctx's deadline, never negative.
func waitBudget(ctx context.Context, d time.Duration) time.Duration {
	if ctx.Err() != nil {
		return 0
	}
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl); rem < d {
			if rem < 0 {
				return 0
			}
			return rem
		}
	}
	return d
}

func waitForDevice(ctx context.Context, c *client.Client, name string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if slices.Contains(c.Devices(), name) {
			return true
		}
		if ctx.Err() != nil || c.Err() != nil || !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

const settleQuiet = 200 * time.Millisecond

// settle waits until fingerprint holds steady for settleQuiet. fingerprint counts
// structure only, so a stream of value updates cannot keep it waiting forever.
func settle(ctx context.Context, max time.Duration, fingerprint func() int) {
	deadline := time.Now().Add(waitBudget(ctx, max))
	last, lastChange := fingerprint(), time.Now()
	for time.Since(lastChange) < settleQuiet && time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
		if cur := fingerprint(); cur != last {
			last, lastChange = cur, time.Now()
		}
	}
}

func checkDevice(ctx context.Context, c *client.Client, dev string, opts Options, r *report) {
	// Anchor on CONNECTION so the snapshot below is not taken mid-burst.
	c.Wait(dev, "CONNECTION", func(client.Property) bool { return true }, waitBudget(ctx, opts.Timeout))

	props := byName(c.Properties(dev))
	checkProtocol(props, r)

	iface := 0
	advertised := false
	if di, ok := props["DRIVER_INFO"]; ok {
		r.pass("DRIVER_INFO present")
		if m, ok := di.Member("DRIVER_INTERFACE"); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(m.Value)); err == nil {
				iface = n
				advertised = true
				r.info("DRIVER_INTERFACE", interfaceList(iface))
			} else {
				r.fail("DRIVER_INTERFACE numeric", "value "+m.Value)
			}
		} else {
			r.fail("DRIVER_INTERFACE present", "DRIVER_INFO lacks DRIVER_INTERFACE")
		}
	} else {
		r.warn("DRIVER_INFO present", "device does not advertise DRIVER_INFO")
	}

	conn, hasConn := props["CONNECTION"]
	if hasConn {
		r.want("CONNECTION is switch rw", conn.Type == "Switch" && conn.Perm == "rw",
			fmt.Sprintf("type=%s perm=%s", conn.Type, conn.Perm))
		r.want("CONNECTION members", hasAll(conn, "CONNECT", "DISCONNECT"), missingMsg(conn, "CONNECT", "DISCONNECT"))
		r.want("CONNECTION rule OneOfMany", conn.Rule == "OneOfMany", "rule="+conn.Rule)
	} else {
		r.fail("CONNECTION present", "no CONNECTION property")
	}

	connected := hasConn && on(conn, "CONNECT")
	connectedByUs := false
	if opts.Mutate && hasConn && !connected {
		connectedByUs = connectCheck(ctx, c, dev, opts, r)
		connected = connectedByUs
	}
	if connected {
		// Drivers commonly define their interface properties only after CONNECT, so
		// re-snapshot once that second def burst goes quiet.
		settle(ctx, opts.Timeout, func() int { return len(c.Properties(dev)) })
		props = byName(c.Properties(dev))
	}

	switch {
	case !advertised:
		r.info("interface contracts skipped", "no DRIVER_INTERFACE advertised; interface contract checks not run")
	case hasConn && !connected && !opts.Mutate && iface&contractBits != 0:
		r.info("interface contracts skipped",
			"device not connected; interface properties are typically defined only after CONNECT — re-run with -mutate to connect and check them")
	default:
		if hasConn && !connected && opts.Mutate && iface&contractBits != 0 {
			r.info("interface contracts", "device did not connect; contracts evaluated on the pre-connect property set")
		}
		// A device with no CONNECTION has nothing to connect, so it counts as live.
		checkContracts(ctx, c, dev, iface, props, connected || !hasConn, opts, r)
	}

	// Leave the device as found.
	if connectedByUs {
		disconnectCheck(ctx, c, dev, opts, r)
	}
}

func checkProtocol(props map[string]client.Property, r *report) {
	for _, name := range sortedNames(props) {
		p := props[name]
		if !validState[p.State] {
			r.fail("state valid: "+p.Name, "state="+p.State)
		}
		// defLightVector carries no perm attribute per the DTD, so an empty one is correct.
		if p.Type != "Light" && !validPerm[p.Perm] {
			r.fail("perm valid: "+p.Name, "perm="+p.Perm)
		}
		// A Switch vector's rule is required, so validRule deliberately excludes "".
		if p.Type == "Switch" && !validRule[p.Rule] {
			r.fail("switch rule valid: "+p.Name, "rule="+p.Rule)
		}
		if p.Type == "Number" {
			for _, m := range p.Members {
				// Per INDI convention min == max means "no limit", so only min > max is invalid.
				if m.Min > m.Max {
					r.fail("number range: "+p.Name+"."+m.Name, fmt.Sprintf("min %g > max %g", m.Min, m.Max))
				}
			}
		}
	}
}

// connectCheck sends CONNECT and reports whether conform itself connected the device,
// in which case the caller owes it a DISCONNECT.
func connectCheck(ctx context.Context, c *client.Client, dev string, opts Options, r *report) bool {
	p, err := c.SetSwitchAndWait(dev, "CONNECTION", map[string]bool{"CONNECT": true}, waitBudget(ctx, opts.Timeout))
	switch {
	case err != nil:
		r.fail("CONNECT settles Ok", err.Error())
		return false
	case !on(p, "CONNECT"):
		r.fail("CONNECT settles Ok", "CONNECTION settled Ok but CONNECT is not On")
		return false
	}
	r.pass("CONNECT settles Ok")
	r.want("CONNECTION OneOfMany invariant", on(p, "CONNECT") != on(p, "DISCONNECT"),
		"both or neither of CONNECT/DISCONNECT are On")
	return true
}

// disconnectCheck restores the device to its pre-run state after conform connected it.
//
// # IPS_IDLE is a terminal state for CONNECTION
//
// This cannot use SetSwitchAndWait, and the reason is a property of the reference implementation
// rather than a preference. libindi acknowledges a SUCCESSFUL disconnect with IPS_IDLE:
//
//	// defaultdevice.cpp, the DISCONNECT branch
//	if (Disconnect())
//	{
//	    setConnected(false, IPS_IDLE);
//	    updateProperties();
//	}
//
// SetSwitchAndWait waits for Ok or Alert, which is right for every other vector and wrong for this
// one — so the wait ran to the full timeout on a device that had disconnected instantly, and this
// check warned about all eight libindi simulators. A validator that reports the reference
// implementation as non-conforming is worse than no validator: it teaches its users to ignore it.
//
// Busy is the only state meaning "still working", so anything else is an answer.
func disconnectCheck(ctx context.Context, c *client.Client, dev string, opts Options, r *report) {
	check := "DISCONNECT restores initial state"
	var since uint64
	if cur, ok := c.Property(dev, "CONNECTION"); ok {
		since = cur.Rev // recorded before commanding, so only a later update satisfies the wait
	}
	if err := c.SetSwitch(dev, "CONNECTION", map[string]bool{"DISCONNECT": true}); err != nil {
		r.fail(check, err.Error())
		return
	}
	p, err := c.WaitRev(dev, "CONNECTION", since, func(p client.Property) bool {
		return p.State != "Busy"
	}, waitBudget(ctx, opts.Timeout))
	switch {
	case err != nil:
		r.warn(check, "CONNECTION did not settle after DISCONNECT: "+err.Error())
	case p.State == "Alert":
		r.fail(check, "CONNECTION settled Alert after DISCONNECT: "+p.Message)
	case !on(p, "DISCONNECT"):
		r.fail(check, "CONNECTION settled but DISCONNECT is not On")
	default:
		r.pass(check)
	}
}

// checkContracts runs the per-interface property contracts; live gates the
// state-changing sub-checks.
func checkContracts(ctx context.Context, c *client.Client, dev string, iface int, props map[string]client.Property, live bool, opts Options, r *report) {
	if iface&ifTelescope != 0 {
		requireProp(props, "EQUATORIAL_EOD_COORD", "Number", "rw", []string{"RA", "DEC"}, r)
		requireProp(props, "ON_COORD_SET", "Switch", "rw", []string{"SLEW", "TRACK", "SYNC"}, r)
		requireProp(props, "TELESCOPE_ABORT_MOTION", "Switch", "rw", []string{"ABORT"}, r)
	}
	if iface&ifGuider != 0 {
		requireProp(props, "TELESCOPE_TIMED_GUIDE_NS", "Number", "rw", []string{"TIMED_GUIDE_N", "TIMED_GUIDE_S"}, r)
		requireProp(props, "TELESCOPE_TIMED_GUIDE_WE", "Number", "rw", []string{"TIMED_GUIDE_W", "TIMED_GUIDE_E"}, r)
		if opts.Mutate && live {
			pulseCheck(ctx, c, dev, "TELESCOPE_TIMED_GUIDE_NS", "TIMED_GUIDE_N", opts, r)
			pulseCheck(ctx, c, dev, "TELESCOPE_TIMED_GUIDE_WE", "TIMED_GUIDE_W", opts, r)
		}
	}
	if iface&ifCCD != 0 {
		checkCCD(ctx, c, dev, props, live, opts, r)
	}
	if iface&ifFocuser != 0 {
		_, abs := props["ABS_FOCUS_POSITION"]
		_, motion := props["FOCUS_MOTION"]
		_, timer := props["FOCUS_TIMER"]
		r.want("FOCUSER contract", abs || (motion && timer),
			"need ABS_FOCUS_POSITION or FOCUS_MOTION+FOCUS_TIMER")
	}
	if iface&ifFilter != 0 {
		requireProp(props, "FILTER_SLOT", "Number", "rw", nil, r)
	}
	if iface&ifDome != 0 {
		_, motion := props["DOME_MOTION"]
		_, abs := props["ABS_DOME_POSITION"]
		r.want("DOME contract", motion || abs, "need DOME_MOTION or ABS_DOME_POSITION")
	}
	if iface&ifGPS != 0 {
		requirePresent(props, "GEOGRAPHIC_COORD", r)
		requirePresent(props, "TIME_UTC", r)
	}
	if iface&ifWeather != 0 {
		if p, ok := props["WEATHER_STATUS"]; !ok {
			r.fail("WEATHER_STATUS present", "required property missing")
		} else {
			r.pass("WEATHER_STATUS present")
			r.want("WEATHER_STATUS is Light", p.Type == "Light", "type="+p.Type)
		}
	}
	if iface&ifRotator != 0 {
		requireProp(props, "ABS_ROTATOR_ANGLE", "Number", "rw", nil, r)
	}
}

// checkCCD validates the camera contract: an exposure property, the sensor geometry,
// and a BLOB vector to carry frames.
func checkCCD(ctx context.Context, c *client.Client, dev string, props map[string]client.Property, live bool, opts Options, r *report) {
	requireProp(props, "CCD_EXPOSURE", "Number", "rw", []string{"CCD_EXPOSURE_VALUE"}, r)
	requireProp(props, "CCD_INFO", "Number", "ro",
		[]string{"CCD_PIXEL_SIZE", "CCD_PIXEL_SIZE_X", "CCD_PIXEL_SIZE_Y", "CCD_MAX_X", "CCD_MAX_Y"}, r)

	blobName := ""
	if p, ok := props["CCD1"]; ok && p.Type == "BLOB" {
		blobName = "CCD1"
	} else {
		for _, name := range sortedNames(props) {
			if props[name].Type == "BLOB" {
				blobName = name
				break
			}
		}
	}
	switch {
	case blobName == "":
		r.fail("CCD BLOB vector present", "no BLOB property to carry frames (conventionally CCD1)")
	case blobName != "CCD1":
		r.add("CCD BLOB vector present", Pass, "property "+blobName+" (conventionally CCD1)")
	default:
		r.pass("CCD BLOB vector present")
	}

	if opts.Mutate && live {
		exposeCheck(ctx, c, dev, props, opts, r)
	}
}

// exposeCheck takes the shortest allowed exposure and requires a non-empty FITS payload.
func exposeCheck(ctx context.Context, c *client.Client, dev string, props map[string]client.Property, opts Options, r *report) {
	check := "CCD exposure delivers FITS BLOB"
	exp, ok := props["CCD_EXPOSURE"]
	if !ok {
		return // already failed in checkCCD
	}
	dur := 0.1
	if m, ok := exp.Member("CCD_EXPOSURE_VALUE"); ok && m.Min > dur {
		dur = m.Min
	}

	type delivered struct {
		info client.BlobInfo
		data []byte
	}
	ch := make(chan delivered, 4)
	c.BufferBlobs(func(info client.BlobInfo, data []byte) {
		if info.Device != dev {
			return
		}
		select {
		case ch <- delivered{info, data}:
		default:
		}
	})
	defer c.BlobSink(nil)
	if err := c.EnableBLOB(dev, "", client.BlobAlso); err != nil {
		r.fail(check, "enableBLOB: "+err.Error())
		return
	}

	if _, err := c.SetNumberAndWait(dev, "CCD_EXPOSURE",
		map[string]float64{"CCD_EXPOSURE_VALUE": dur}, waitBudget(ctx, opts.Timeout)); err != nil {
		r.fail(check, "CCD_EXPOSURE did not settle Ok: "+err.Error())
		return
	}
	timer := time.NewTimer(waitBudget(ctx, opts.Timeout))
	defer timer.Stop()
	var got delivered
	select {
	case got = <-ch:
	case <-timer.C:
		r.fail(check, "no BLOB delivered after the exposure settled Ok")
		return
	case <-ctx.Done():
		r.fail(check, "aborted: "+ctx.Err().Error())
		return
	}

	switch {
	case len(got.data) == 0:
		r.fail(check, "empty BLOB payload")
	case !strings.Contains(strings.ToLower(got.info.Format), "fits"):
		r.fail(check, fmt.Sprintf("format %q does not look like FITS", got.info.Format))
	default:
		r.pass(check)
	}

	// Per the DTD the size attr counts decoded AND uncompressed bytes.
	if got.info.Size > 0 {
		n := int64(len(got.data))
		if got.info.Compressed {
			m, err := inflatedLen(got.data)
			if err != nil {
				r.fail("CCD BLOB size attr consistent", "compressed payload does not inflate: "+err.Error())
				return
			}
			n = m
		}
		r.want("CCD BLOB size attr consistent", n == got.info.Size,
			fmt.Sprintf("size attr %d != decoded/uncompressed payload %d bytes", got.info.Size, n))
	}
}

func inflatedLen(data []byte) (int64, error) {
	zr, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return 0, err
	}
	defer zr.Close()
	return io.Copy(io.Discard, zr)
}

// pulseCheck issues a 10ms guide pulse and reports whether the device acknowledges it.
// pulseCheck commands a short guide pulse. A timed guide is an INITIATOR, so the check is that the
// driver ACCEPTED it — not that the vector reached Ok, which it never does.
//
// libindi's GuiderInterface starts a pulse by putting the vector in IPS_BUSY and finishes it with
// IPS_IDLE (`indiguiderinterface.cpp:127`: `GuideNSNP.setState(IPS_IDLE)` in GuideComplete). Ok is
// not in that sequence at all, so SetNumberAndWait — which waits for Ok or Alert — ran to the full
// timeout on a pulse that had been accepted and completed normally, and warned about the reference
// implementation.
//
// A 10 ms pulse can also be OVER before the wait starts, so Busy and Idle are both passes: the
// first is "running", the second is "ran". Only Alert is a refusal.
func pulseCheck(ctx context.Context, c *client.Client, dev, prop, member string, opts Options, r *report) {
	check := "pulse guide accepted: " + prop
	before := len(c.Messages())
	var since uint64
	if cur, ok := c.Property(dev, prop); ok {
		since = cur.Rev
	}
	var p client.Property
	err := c.SetNumber(dev, prop, map[string]float64{member: 10})
	if err == nil {
		p, err = c.WaitRev(dev, prop, since, func(p client.Property) bool {
			return p.State != "Busy" || on(p, member)
		}, waitBudget(ctx, opts.Timeout))
	}
	switch {
	case err != nil && p.State == "Alert":
		r.fail(check, err.Error())
	case err != nil:
		r.warn(check, prop+" was not acknowledged: "+err.Error())
	case p.State == "Alert":
		r.fail(check, prop+" refused the pulse: "+p.Message)
	default:
		r.pass(check)
	}
	if msgs := c.Messages(); len(msgs) > before {
		r.info("pulse guide messages: "+prop, fmt.Sprintf("%v", msgs[before:]))
	}
}

func requireProp(props map[string]client.Property, name, typ, perm string, members []string, r *report) {
	p, ok := props[name]
	if !ok {
		r.fail(name+" present", "required property missing")
		return
	}
	r.want(name+" type/perm", p.Type == typ && p.Perm == perm, fmt.Sprintf("type=%s perm=%s (want %s/%s)", p.Type, p.Perm, typ, perm))
	if len(members) == 0 {
		return
	}
	if miss := missing(p, members); len(miss) > 0 {
		r.fail(name+" members", fmt.Sprintf("missing %v", miss))
	} else {
		r.pass(name + " members")
	}
}

func requirePresent(props map[string]client.Property, name string, r *report) {
	_, ok := props[name]
	r.want(name+" present", ok, "required property missing")
}

func byName(ps []client.Property) map[string]client.Property {
	m := make(map[string]client.Property, len(ps))
	for _, p := range ps {
		m[p.Name] = p
	}
	return m
}

func sortedNames(props map[string]client.Property) []string {
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func hasAll(p client.Property, names ...string) bool { return len(missing(p, names)) == 0 }

func missing(p client.Property, names []string) []string {
	var miss []string
	for _, n := range names {
		if _, ok := p.Member(n); !ok {
			miss = append(miss, n)
		}
	}
	return miss
}

func missingMsg(p client.Property, names ...string) string {
	if m := missing(p, names); len(m) > 0 {
		return fmt.Sprintf("missing %v", m)
	}
	return ""
}

func on(p client.Property, name string) bool {
	if m, ok := p.Member(name); ok {
		return m.On
	}
	return false
}

// Indexed by bit position, so the order is load-bearing: bit 0 = TELESCOPE .. bit 15 = AUX.
var interfaceNames = []string{
	"TELESCOPE", "CCD", "GUIDER", "FOCUSER", "FILTER", "DOME", "GPS", "WEATHER",
	"AO", "DUSTCAP", "LIGHTBOX", "DETECTOR", "ROTATOR", "SPECTROGRAPH", "CORRELATOR", "AUX",
}

func interfaceList(iface int) string {
	if iface == 0 {
		return "0 [GENERAL]"
	}
	var s []string
	for i, name := range interfaceNames {
		if iface&(1<<i) != 0 {
			s = append(s, name)
		}
	}
	return fmt.Sprintf("%d %v", iface, s)
}

// Summarize counts results by status.
func Summarize(results []Result) (pass, fail, warn, info int) {
	for _, r := range results {
		switch r.Status {
		case Pass:
			pass++
		case Fail:
			fail++
		case Warn:
			warn++
		case Info:
			info++
		}
	}
	return
}

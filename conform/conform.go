// Package conform is the ConformU analogue for INDI: it drives an INDI server
// through the goindi client and reports whether it conforms to the protocol and
// the standard property contracts the cross-platform clients (PHD2, Ekos) rely
// on. It is a black-box validator — point it at any INDI server, not just ours.
package conform

import (
	"fmt"
	"strconv"
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
	Mutate  bool          // run state-changing checks (connect, guide pulse)
	Timeout time.Duration // per-wait timeout (default 3s)
}

// DRIVER_INTERFACE bits (mirrors server.Interface*).
const (
	ifTelescope = 1 << 0
	ifCCD       = 1 << 1
	ifGuider    = 1 << 2
)

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
	validRule  = map[string]bool{"OneOfMany": true, "AtMostOne": true, "AnyOfMany": true, "": true}
)

// Run executes the conformance battery against c and returns the results.
func Run(c *client.Client, opts Options) []Result {
	if opts.Timeout == 0 {
		opts.Timeout = 3 * time.Second
	}
	r := &report{}

	_ = c.GetProperties("", "")
	if !c.WaitDevices(1, opts.Timeout) {
		r.fail("discovery", "no devices defined after getProperties")
		return r.results
	}
	r.info("discovery", fmt.Sprintf("devices: %v", c.Devices()))

	for _, dev := range c.Devices() {
		if opts.Device != "" && dev != opts.Device {
			continue
		}
		r.dev = dev
		checkDevice(c, dev, opts, r)
	}
	r.dev = ""
	return r.results
}

func checkDevice(c *client.Client, dev string, opts Options, r *report) {
	// Anchor: CONNECTION should arrive; wait for it so the snapshot is complete.
	c.Wait(dev, "CONNECTION", func(client.Property) bool { return true }, opts.Timeout)

	props := byName(c.Properties(dev))

	// --- protocol-level checks on every property ---
	checkProtocol(props, r)

	// --- DRIVER_INFO / interface ---
	iface := 0
	if di, ok := props["DRIVER_INFO"]; ok {
		r.pass("DRIVER_INFO present")
		if m, ok := di.Member("DRIVER_INTERFACE"); ok {
			if n, err := strconv.Atoi(trim(m.Value)); err == nil {
				iface = n
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

	// --- CONNECTION contract (every device) ---
	if p, ok := props["CONNECTION"]; ok {
		r.want("CONNECTION is switch rw", p.Type == "Switch" && p.Perm == "rw",
			fmt.Sprintf("type=%s perm=%s", p.Type, p.Perm))
		r.want("CONNECTION members", hasAll(p, "CONNECT", "DISCONNECT"), missingMsg(p, "CONNECT", "DISCONNECT"))
		r.want("CONNECTION rule OneOfMany", p.Rule == "OneOfMany", "rule="+p.Rule)
	} else {
		r.fail("CONNECTION present", "no CONNECTION property")
	}

	// --- telescope contract ---
	if iface&ifTelescope != 0 {
		requireProp(props, "EQUATORIAL_EOD_COORD", "Number", "rw", []string{"RA", "DEC"}, r)
		requireProp(props, "ON_COORD_SET", "Switch", "rw", []string{"SLEW", "TRACK", "SYNC"}, r)
		requireProp(props, "TELESCOPE_ABORT_MOTION", "Switch", "rw", []string{"ABORT"}, r)
	}

	// --- guider contract ---
	if iface&ifGuider != 0 {
		requireProp(props, "TELESCOPE_TIMED_GUIDE_NS", "Number", "rw", []string{"TIMED_GUIDE_N", "TIMED_GUIDE_S"}, r)
		requireProp(props, "TELESCOPE_TIMED_GUIDE_WE", "Number", "rw", []string{"TIMED_GUIDE_W", "TIMED_GUIDE_E"}, r)
	}

	if opts.Mutate {
		checkBehavior(c, dev, iface, opts, r)
	}
}

func checkProtocol(props map[string]client.Property, r *report) {
	for _, p := range props {
		if !validState[p.State] {
			r.fail("state valid: "+p.Name, "state="+p.State)
		}
		if !validPerm[p.Perm] {
			r.fail("perm valid: "+p.Name, "perm="+p.Perm)
		}
		if p.Type == "Switch" && !validRule[p.Rule] {
			r.fail("switch rule valid: "+p.Name, "rule="+p.Rule)
		}
		if p.Type == "Number" {
			for _, m := range p.Members {
				if m.Max != 0 && m.Min > m.Max {
					r.fail("number range: "+p.Name+"."+m.Name, fmt.Sprintf("min %g > max %g", m.Min, m.Max))
				}
			}
		}
	}
}

func checkBehavior(c *client.Client, dev string, iface int, opts Options, r *report) {
	// Connect, expect CONNECTION to settle Ok with CONNECT On.
	if _, ok := c.Property(dev, "CONNECTION"); ok {
		_ = c.SetSwitch(dev, "CONNECTION", map[string]bool{"CONNECT": true})
		p, ok := c.Wait(dev, "CONNECTION", func(p client.Property) bool {
			return p.State == "Ok" && on(p, "CONNECT")
		}, opts.Timeout)
		r.want("CONNECT settles Ok", ok, "CONNECTION did not reach Ok/CONNECT within timeout")
		if ok {
			// OneOfMany invariant: exactly one of CONNECT/DISCONNECT On.
			r.want("CONNECTION OneOfMany invariant", on(p, "CONNECT") != on(p, "DISCONNECT"),
				"both or neither of CONNECT/DISCONNECT are On")
		}
	}

	// Guider: a tiny pulse should be accepted (no Alert, no error message).
	if iface&ifGuider != 0 {
		before := len(c.Messages())
		_ = c.SetNumber(dev, "TELESCOPE_TIMED_GUIDE_NS", map[string]float64{"TIMED_GUIDE_N": 10})
		p, ok := c.Wait(dev, "TELESCOPE_TIMED_GUIDE_NS", func(p client.Property) bool {
			return p.State == "Ok"
		}, opts.Timeout)
		switch {
		case !ok && p.State == "Alert":
			r.fail("pulse guide accepted", "TELESCOPE_TIMED_GUIDE_NS went to Alert")
		case !ok:
			r.warn("pulse guide accepted", "TELESCOPE_TIMED_GUIDE_NS did not settle Ok (state="+p.State+")")
		default:
			r.pass("pulse guide accepted")
		}
		if len(c.Messages()) > before {
			r.info("pulse guide messages", fmt.Sprintf("%v", c.Messages()[before:]))
		}
	}
}

func requireProp(props map[string]client.Property, name, typ, perm string, members []string, r *report) {
	p, ok := props[name]
	if !ok {
		r.fail(name+" present", "required property missing")
		return
	}
	r.want(name+" type/perm", p.Type == typ && p.Perm == perm, fmt.Sprintf("type=%s perm=%s (want %s/%s)", p.Type, p.Perm, typ, perm))
	if miss := missing(p, members); len(miss) > 0 {
		r.fail(name+" members", fmt.Sprintf("missing %v", miss))
	} else {
		r.pass(name + " members")
	}
}

// ---- helpers ----

func byName(ps []client.Property) map[string]client.Property {
	m := make(map[string]client.Property, len(ps))
	for _, p := range ps {
		m[p.Name] = p
	}
	return m
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

func interfaceList(iface int) string {
	var s []string
	if iface&ifTelescope != 0 {
		s = append(s, "TELESCOPE")
	}
	if iface&ifCCD != 0 {
		s = append(s, "CCD")
	}
	if iface&ifGuider != 0 {
		s = append(s, "GUIDER")
	}
	return fmt.Sprintf("%d %v", iface, s)
}

func trim(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\n' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\n' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
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

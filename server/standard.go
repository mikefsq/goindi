package server

import "strconv"

// INDI DRIVER_INTERFACE bitmask values (subset). A device advertises the sum of
// the interfaces it implements in DRIVER_INFO so clients categorize it — PHD2's
// mount dropdown shows TELESCOPE devices, its camera dropdown CCD devices.
const (
	InterfaceGeneral   = 0
	InterfaceTelescope = 1 << 0 // 1
	InterfaceCCD       = 1 << 1 // 2
	InterfaceGuider    = 1 << 2 // 4
	InterfaceFocuser   = 1 << 3 // 8
	InterfaceFilter    = 1 << 4 // 16
)

func numMember(name, label, format string, min, max, step, val float64) *Member {
	return &Member{Name: name, Label: label, Format: format, Min: min, Max: max, Step: step, Num: val}
}
func swMember(name, label string, on bool) *Member { return &Member{Name: name, Label: label, On: on} }
func txtMember(name, label, val string) *Member    { return &Member{Name: name, Label: label, Text: val} }

// --- Standard telescope properties (the set PHD2 / Ekos drive) ---

// ConnectionProperty is the universal CONNECT/DISCONNECT switch (defaults disconnected).
func ConnectionProperty(device string) *Property {
	p := NewProperty(device, "CONNECTION", SwitchType, RW,
		swMember("CONNECT", "Connect", false),
		swMember("DISCONNECT", "Disconnect", true))
	p.Label, p.Group, p.Rule = "Connection", "Main Control", OneOfMany
	p.SetState(Idle)
	return p
}

// DriverInfoProperty advertises the device name/exec/version and its interface mask.
func DriverInfoProperty(device, name, exec, version string, iface int) *Property {
	p := NewProperty(device, "DRIVER_INFO", TextType, RO,
		txtMember("DRIVER_NAME", "Name", name),
		txtMember("DRIVER_EXEC", "Exec", exec),
		txtMember("DRIVER_VERSION", "Version", version),
		txtMember("DRIVER_INTERFACE", "Interface", strconv.Itoa(iface)))
	p.Label, p.Group = "Driver Info", "General Info"
	p.SetState(Ok)
	return p
}

// EquatorialCoordProperty is the live RA (hours) / DEC (degrees) vector; writing it
// slews or syncs per ON_COORD_SET.
func EquatorialCoordProperty(device string) *Property {
	p := NewProperty(device, "EQUATORIAL_EOD_COORD", NumberType, RW,
		numMember("RA", "RA (hours)", "%10.6m", 0, 24, 0, 0),
		numMember("DEC", "DEC (deg)", "%10.6m", -90, 90, 0, 0))
	p.Label, p.Group = "Eq. Coordinates", "Main Control"
	p.SetState(Idle)
	return p
}

// OnCoordSetProperty selects what writing EQUATORIAL_EOD_COORD does (defaults TRACK).
func OnCoordSetProperty(device string) *Property {
	p := NewProperty(device, "ON_COORD_SET", SwitchType, RW,
		swMember("SLEW", "Slew", false),
		swMember("TRACK", "Track", true),
		swMember("SYNC", "Sync", false))
	p.Label, p.Group, p.Rule = "On Set", "Main Control", OneOfMany
	p.SetState(Ok)
	return p
}

// AbortProperty stops all motion when ABORT is set On (momentary).
func AbortProperty(device string) *Property {
	p := NewProperty(device, "TELESCOPE_ABORT_MOTION", SwitchType, RW,
		swMember("ABORT", "Abort", false))
	p.Label, p.Group, p.Rule = "Abort Motion", "Main Control", AtMostOne
	p.SetState(Ok)
	return p
}

// PierSideProperty reports the German-equatorial pier side (read-only).
func PierSideProperty(device string) *Property {
	p := NewProperty(device, "TELESCOPE_PIER_SIDE", SwitchType, RO,
		swMember("PIER_WEST", "West (pointing east)", false),
		swMember("PIER_EAST", "East (pointing west)", false))
	p.Label, p.Group, p.Rule = "Pier Side", "Main Control", AtMostOne
	p.SetState(Idle)
	return p
}

// TimedGuideNSProperty / TimedGuideWEProperty are the pulse-guide vectors (milliseconds)
// — the properties PHD2 writes to guide.
func TimedGuideNSProperty(device string) *Property {
	p := NewProperty(device, "TELESCOPE_TIMED_GUIDE_NS", NumberType, RW,
		numMember("TIMED_GUIDE_N", "North (ms)", "%.0f", 0, 60000, 1, 0),
		numMember("TIMED_GUIDE_S", "South (ms)", "%.0f", 0, 60000, 1, 0))
	p.Label, p.Group = "Guide N/S", "Guide"
	p.SetState(Ok)
	return p
}

func TimedGuideWEProperty(device string) *Property {
	p := NewProperty(device, "TELESCOPE_TIMED_GUIDE_WE", NumberType, RW,
		numMember("TIMED_GUIDE_W", "West (ms)", "%.0f", 0, 60000, 1, 0),
		numMember("TIMED_GUIDE_E", "East (ms)", "%.0f", 0, 60000, 1, 0))
	p.Label, p.Group = "Guide W/E", "Guide"
	p.SetState(Ok)
	return p
}

// GuideRateProperty reports the mount's guide rate as a fraction of sidereal (0.5 =
// half), which PHD2 reads (the GUIDE_RATE property, members GUIDE_RATE_WE/NS) to
// scale its calibration; without it PHD2 warns and falls back to 0.5x.
func GuideRateProperty(device string, rate float64) *Property {
	p := NewProperty(device, "GUIDE_RATE", NumberType, RW,
		numMember("GUIDE_RATE_WE", "RA (x sidereal)", "%.2f", 0, 1, 0.05, rate),
		numMember("GUIDE_RATE_NS", "DEC (x sidereal)", "%.2f", 0, 1, 0.05, rate))
	p.Label, p.Group = "Guide Rate", "Guide"
	p.SetState(Ok)
	return p
}

// DualAxisTrackingProperty toggles dual-axis tracking — the mount driving BOTH axes to
// follow its refraction/pointing model. Only mounts that support it (lx200.DualAxisTracker,
// e.g. 10Micron :Sdat/:Gdat) get this property: the driver defines it on connect and
// removes it on disconnect. OneOfMany ENABLE/DISABLE.
func DualAxisTrackingProperty(device string) *Property {
	p := NewProperty(device, "DUAL_AXIS_TRACKING", SwitchType, RW,
		swMember("ENABLE", "Enable", false),
		swMember("DISABLE", "Disable", true))
	p.Label, p.Group, p.Rule = "Dual Axis Tracking", "Motion Control", OneOfMany
	p.SetState(Ok)
	return p
}

// TelescopeInfoProperty reports the optical-train parameters in millimetres (the
// main scope and the guide scope). The mount can't measure these — they come from
// the driver's optics config — so a client derives image/pixel scale by combining
// the focal length here with the camera's CCD_INFO pixel size. Read-only: optics is
// set on our side through Alpaca, then reported identically here.
func TelescopeInfoProperty(device string) *Property {
	p := NewProperty(device, "TELESCOPE_INFO", NumberType, RO,
		numMember("TELESCOPE_APERTURE", "Aperture (mm)", "%.2f", 0, 1e6, 0, 0),
		numMember("TELESCOPE_FOCAL_LENGTH", "Focal Length (mm)", "%.2f", 0, 1e6, 0, 0),
		numMember("GUIDER_APERTURE", "Guider Aperture (mm)", "%.2f", 0, 1e6, 0, 0),
		numMember("GUIDER_FOCAL_LENGTH", "Guider Focal Length (mm)", "%.2f", 0, 1e6, 0, 0))
	p.Label, p.Group = "Scope Properties", "Options"
	p.SetState(Ok)
	return p
}

package server

import "strconv"

// INDI DRIVER_INTERFACE bitmask values; a device advertises the sum of the
// interfaces it implements in DRIVER_INFO.
const (
	InterfaceGeneral      = 0
	InterfaceTelescope    = 1 << 0
	InterfaceCCD          = 1 << 1
	InterfaceGuider       = 1 << 2
	InterfaceFocuser      = 1 << 3
	InterfaceFilter       = 1 << 4
	InterfaceDome         = 1 << 5
	InterfaceGPS          = 1 << 6
	InterfaceWeather      = 1 << 7
	InterfaceAO           = 1 << 8
	InterfaceDustcap      = 1 << 9
	InterfaceLightbox     = 1 << 10
	InterfaceDetector     = 1 << 11
	InterfaceRotator      = 1 << 12
	InterfaceSpectrograph = 1 << 13
	InterfaceCorrelator   = 1 << 14
	InterfaceAux          = 1 << 15
)

func numMember(name, label, format string, min, max, step, val float64) *Member {
	return &Member{Name: name, Label: label, Format: format, Min: min, Max: max, Step: step, Num: val}
}
func swMember(name, label string, on bool) *Member { return &Member{Name: name, Label: label, On: on} }
func txtMember(name, label, val string) *Member    { return &Member{Name: name, Label: label, Text: val} }

// ConnectionProperty is the universal CONNECT/DISCONNECT switch, defaulting to
// disconnected.
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

// EquatorialCoordProperty is the live RA (hours) / DEC (degrees) vector, whose
// writes slew or sync per ON_COORD_SET.
func EquatorialCoordProperty(device string) *Property {
	p := NewProperty(device, "EQUATORIAL_EOD_COORD", NumberType, RW,
		numMember("RA", "RA (hours)", "%10.6m", 0, 24, 0, 0),
		numMember("DEC", "DEC (deg)", "%10.6m", -90, 90, 0, 0))
	p.Label, p.Group = "Eq. Coordinates", "Main Control"
	p.SetState(Idle)
	return p
}

// OnCoordSetProperty selects what a write to EQUATORIAL_EOD_COORD does,
// defaulting to TRACK.
func OnCoordSetProperty(device string) *Property {
	p := NewProperty(device, "ON_COORD_SET", SwitchType, RW,
		swMember("SLEW", "Slew", false),
		swMember("TRACK", "Track", true),
		swMember("SYNC", "Sync", false))
	p.Label, p.Group, p.Rule = "On Set", "Main Control", OneOfMany
	p.SetState(Ok)
	return p
}

// AbortProperty stops all motion when ABORT is set On.
func AbortProperty(device string) *Property {
	p := NewProperty(device, "TELESCOPE_ABORT_MOTION", SwitchType, RW,
		swMember("ABORT", "Abort", false))
	p.Label, p.Group, p.Rule = "Abort Motion", "Main Control", AtMostOne
	p.SetState(Ok)
	return p
}

// PierSideProperty reports the German-equatorial pier side.
func PierSideProperty(device string) *Property {
	p := NewProperty(device, "TELESCOPE_PIER_SIDE", SwitchType, RO,
		swMember("PIER_WEST", "West (pointing east)", false),
		swMember("PIER_EAST", "East (pointing west)", false))
	p.Label, p.Group, p.Rule = "Pier Side", "Main Control", AtMostOne
	p.SetState(Idle)
	return p
}

// TimedGuideNSProperty is the north/south pulse-guide vector, in milliseconds.
func TimedGuideNSProperty(device string) *Property {
	p := NewProperty(device, "TELESCOPE_TIMED_GUIDE_NS", NumberType, RW,
		numMember("TIMED_GUIDE_N", "North (ms)", "%.0f", 0, 60000, 1, 0),
		numMember("TIMED_GUIDE_S", "South (ms)", "%.0f", 0, 60000, 1, 0))
	p.Label, p.Group = "Guide N/S", "Guide"
	p.SetState(Ok)
	return p
}

// TimedGuideWEProperty is the west/east pulse-guide vector, in milliseconds.
func TimedGuideWEProperty(device string) *Property {
	p := NewProperty(device, "TELESCOPE_TIMED_GUIDE_WE", NumberType, RW,
		numMember("TIMED_GUIDE_W", "West (ms)", "%.0f", 0, 60000, 1, 0),
		numMember("TIMED_GUIDE_E", "East (ms)", "%.0f", 0, 60000, 1, 0))
	p.Label, p.Group = "Guide W/E", "Guide"
	p.SetState(Ok)
	return p
}

// GuideRateProperty reports the mount's guide rate as a fraction of sidereal.
func GuideRateProperty(device string, rate float64) *Property {
	p := NewProperty(device, "GUIDE_RATE", NumberType, RW,
		numMember("GUIDE_RATE_WE", "RA (x sidereal)", "%.2f", 0, 1, 0.05, rate),
		numMember("GUIDE_RATE_NS", "DEC (x sidereal)", "%.2f", 0, 1, 0.05, rate))
	p.Label, p.Group = "Guide Rate", "Guide"
	p.SetState(Ok)
	return p
}

// DualAxisTrackingProperty toggles tracking on both axes, following the mount's
// refraction and pointing model.
func DualAxisTrackingProperty(device string) *Property {
	p := NewProperty(device, "DUAL_AXIS_TRACKING", SwitchType, RW,
		swMember("ENABLE", "Enable", false),
		swMember("DISABLE", "Disable", true))
	p.Label, p.Group, p.Rule = "Dual Axis Tracking", "Motion Control", OneOfMany
	p.SetState(Ok)
	return p
}

// TelescopeInfoProperty reports the main and guide scope optical parameters in
// millimetres.
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

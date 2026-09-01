package client

// Tracking rates, as libindi defines them in indimacros.h.
//
// A driver fills TELESCOPE_TRACK_RATE's TRACK_RATE_RA with TRACKRATE_SIDEREAL
// by default (inditelescope.cpp), so a client comparing a mount's reported rate
// against sidereal needs the same number the driver started from.
//
// Derived rather than written as a decimal: 15.041067 is a rounding of a
// definition, and a client that hardcodes the quotient carries a permanent rate
// offset into every comparison it makes.
const (
	// StellarDaySec is STELLAR_DAY: one stellar day in SI seconds.
	StellarDaySec = 86164.098903691
	// SolarDaySec is SOLAR_DAY.
	SolarDaySec = 86400.0

	// TrackRateSidereal is TRACKRATE_SIDEREAL, in arcsec/s.
	TrackRateSidereal = (360.0 * 3600.0) / StellarDaySec
	// TrackRateSolar is TRACKRATE_SOLAR, in arcsec/s.
	TrackRateSolar = (360.0 * 3600.0) / SolarDaySec
	// TrackRateLunar is TRACKRATE_LUNAR, in arcsec/s. A literal upstream too,
	// not a quotient of anything defined beside it.
	TrackRateLunar = 14.511415
)

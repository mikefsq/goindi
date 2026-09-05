package client

// Time and tracking constants follow libindi's indimacros.h definitions.
const (
	// StellarDaySec is STELLAR_DAY: one stellar day in SI seconds.
	StellarDaySec = 86164.098903691
	// SolarDaySec is SOLAR_DAY.
	SolarDaySec = 86400.0

	// TrackRateSidereal is TRACKRATE_SIDEREAL, in arcsec/s.
	TrackRateSidereal = (360.0 * 3600.0) / StellarDaySec
	// TrackRateSolar is TRACKRATE_SOLAR, in arcsec/s.
	TrackRateSolar = (360.0 * 3600.0) / SolarDaySec
	// TrackRateLunar is TRACKRATE_LUNAR, in arcsec/s.
	TrackRateLunar = 14.511415
)

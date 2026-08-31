package ccd

// Optional Camera capabilities. A camera implementing one gets the matching
// property defined on connect; one that skips it never advertises the property.

// GainController exposes the sensor gain as CCD_CONTROLS.Gain, in the camera's
// native units.
type GainController interface {
	Gain() (value, min, max int)
	SetGain(int) error
}

// OffsetController exposes the sensor offset (black level) as CCD_CONTROLS.Offset.
type OffsetController interface {
	Offset() (value, min, max int)
	SetOffset(int) error
}

// Subframer exposes a region-of-interest readout as CCD_FRAME, in sensor pixels
// bounded by Camera.Size. SetSubframe takes effect on the next exposure.
type Subframer interface {
	Subframe() (x, y, w, h int)
	SetSubframe(x, y, w, h int) error
}

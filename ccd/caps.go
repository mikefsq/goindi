package ccd

// Optional Camera capabilities define additional properties on connection.

// GainController exposes the sensor gain as CCD_CONTROLS.Gain, in the camera's
// native units.
type GainController interface {
	Gain() (value, min, max int)
	SetGain(int) error
}

// OffsetController adds CCD_CONTROLS.Offset when the camera also implements
// GainController.
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

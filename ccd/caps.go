package ccd

// Optional Camera capabilities. A frame source that implements one of these gets the
// matching INDI property defined the moment it connects, so clients (PHD2/Ekos) can
// read and set it. A camera that doesn't implement a capability simply never advertises
// the property — the base Camera surface is unchanged, so simulators stay minimal.

// GainController exposes the sensor gain as CCD_CONTROLS.Gain. value/min/max are the
// current setting and its range in the camera's native units (e.g. ASI gain).
type GainController interface {
	Gain() (value, min, max int)
	SetGain(int) error
}

// OffsetController exposes the sensor offset (black level) as CCD_CONTROLS.Offset.
type OffsetController interface {
	Offset() (value, min, max int)
	SetOffset(int) error
}

// Subframer exposes a region-of-interest readout as CCD_FRAME (X, Y, WIDTH, HEIGHT), so
// PHD2 can guide on a subframe. Coordinates are sensor pixels; the bounds are the full
// sensor (Camera.Size). SetSubframe takes effect on the next exposure.
type Subframer interface {
	Subframe() (x, y, w, h int)
	SetSubframe(x, y, w, h int) error
}

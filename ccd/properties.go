package ccd

import "github.com/mikefsq/goindi/server"

func num(name, label, format string, min, max, val float64) *server.Member {
	return &server.Member{Name: name, Label: label, Format: format, Min: min, Max: max, Num: val}
}

// ccdInfoProperty is the standard CCD_INFO (pixel size + geometry, read-only) PHD2
// reads to compute pixel scale.
func ccdInfoProperty(device string) *server.Property {
	p := server.NewProperty(device, "CCD_INFO", server.NumberType, server.RO,
		num("CCD_MAX_X", "Width", "%.0f", 0, 1e6, 0),
		num("CCD_MAX_Y", "Height", "%.0f", 0, 1e6, 0),
		num("CCD_PIXEL_SIZE", "Pixel (um)", "%.2f", 0, 100, 0),
		num("CCD_PIXEL_SIZE_X", "Pixel X (um)", "%.2f", 0, 100, 0),
		num("CCD_PIXEL_SIZE_Y", "Pixel Y (um)", "%.2f", 0, 100, 0),
		num("CCD_BITSPERPIXEL", "Bits", "%.0f", 0, 64, 0))
	p.Label, p.Group = "CCD Information", "Image Info"
	p.SetState(server.Ok)
	return p
}

// exposureProperty is CCD_EXPOSURE — writing CCD_EXPOSURE_VALUE (seconds) starts an
// exposure; the device holds it Busy until the frame is delivered.
func exposureProperty(device string) *server.Property {
	p := server.NewProperty(device, "CCD_EXPOSURE", server.NumberType, server.RW,
		num("CCD_EXPOSURE_VALUE", "Duration (s)", "%.3f", 0, 3600, 1))
	p.Label, p.Group = "Expose", "Main Control"
	p.SetState(server.Idle)
	return p
}

func abortProperty(device string) *server.Property {
	p := server.NewProperty(device, "CCD_ABORT_EXPOSURE", server.SwitchType, server.RW,
		&server.Member{Name: "ABORT", Label: "Abort"})
	p.Label, p.Group, p.Rule = "Abort", "Main Control", server.AtMostOne
	p.SetState(server.Ok)
	return p
}

func binningProperty(device string) *server.Property {
	p := server.NewProperty(device, "CCD_BINNING", server.NumberType, server.RW,
		num("HOR_BIN", "X", "%.0f", 1, 4, 1),
		num("VER_BIN", "Y", "%.0f", 1, 4, 1))
	p.Label, p.Group = "Binning", "Image Settings"
	p.SetState(server.Ok)
	return p
}

// blobProperty defines the CCD1 BLOB (the frame). Its data is delivered via the
// server's SendBLOB, not stored on the property.
func blobProperty(device string) *server.Property {
	p := server.NewProperty(device, "CCD1", server.BLOBType, server.RO,
		&server.Member{Name: "CCD1", Label: "Image"})
	p.Label, p.Group = "Image Data", "Image Info"
	p.SetState(server.Ok)
	return p
}

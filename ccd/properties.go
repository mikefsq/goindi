package ccd

import "github.com/mikefsq/goindi/server"

func num(name, label, format string, min, max, val float64) *server.Member {
	return &server.Member{Name: name, Label: label, Format: format, Min: min, Max: max, Num: val}
}

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

// Writing CCD_EXPOSURE_VALUE starts an exposure; it stays Busy until the frame lands.
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

// Ranges come from the live camera, so this is built on connect, not at New.
func controlsProperty(device string, gain, gmin, gmax, off, omin, omax int, hasOffset bool) *server.Property {
	members := []*server.Member{
		num("Gain", "Gain", "%.0f", float64(gmin), float64(gmax), float64(gain)),
	}
	if hasOffset {
		members = append(members, num("Offset", "Offset", "%.0f", float64(omin), float64(omax), float64(off)))
	}
	p := server.NewProperty(device, "CCD_CONTROLS", server.NumberType, server.RW, members...)
	p.Label, p.Group = "Controls", "Image Settings"
	p.SetState(server.Ok)
	return p
}

func frameProperty(device string, x, y, w, h, maxW, maxH int) *server.Property {
	p := server.NewProperty(device, "CCD_FRAME", server.NumberType, server.RW,
		num("X", "Left", "%.0f", 0, float64(maxW), float64(x)),
		num("Y", "Top", "%.0f", 0, float64(maxH), float64(y)),
		num("WIDTH", "Width", "%.0f", 1, float64(maxW), float64(w)),
		num("HEIGHT", "Height", "%.0f", 1, float64(maxH), float64(h)))
	p.Label, p.Group = "Frame", "Image Settings"
	p.SetState(server.Ok)
	return p
}

// Frame bytes go out through Server.SendBLOB; nothing is stored on the property.
func blobProperty(device string) *server.Property {
	p := server.NewProperty(device, "CCD1", server.BLOBType, server.RO,
		&server.Member{Name: "CCD1", Label: "Image"})
	p.Label, p.Group = "Image Data", "Image Info"
	p.SetState(server.Ok)
	return p
}

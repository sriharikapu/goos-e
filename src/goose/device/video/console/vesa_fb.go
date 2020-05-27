package console

import (
	"goose/device"
	"goose/device/video/console/font"
	"goose/device/video/console/logo"
	"goose/kernel"
	"goose/kernel/kfmt"
	"goose/kernel/mm"
	"goose/kernel/mm/vmm"
	"goose/multiboot"
	"image/color"
	"io"
	"reflect"
	"unsafe"
)

// VesaFbConsole is a driver for a console backed by a VESA linear framebuffer.
// The driver supports framebuffers with depth 8, 15, 16, 24 and 32 bpp. In
// all framebuffer configurations, the driver exposes a 256-color palette whose
// entries get mapped to the correct pixel format for the framebuffer.
//
// To provide text output, a font needs to be specified via the SetFont method.
type VesaFbConsole struct {
	bpp           uint32
	bytesPerPixel uint32
	fbPhysAddr    uintptr
	fb            []uint8
	colorInfo     *multiboot.FramebufferRGBColorInfo

	// Console dimensions in pixels
	width  uint32
	height uint32

	// offsetY specifies a the pixel offset for the beginning for text.
	// The rows of the framebuffer between 0 and offsetY are reserved and
	// cannot be used for displaying text.
	offsetY uint32

	// Size of a row in bytes
	pitch uint32

	// Console dimensions in characters
	font          *font.Font
	widthInChars  uint32
	heightInChars uint32

	palette   color.Palette
	defaultFg uint8
	defaultBg uint8
	clearChar uint16
}

// NewVesaFbConsole returns a new instance of the vesa framebuffer driver.
func NewVesaFbConsole(width, height uint32, bpp uint8, pitch uint32, colorInfo *multiboot.FramebufferRGBColorInfo, fbPhysAddr uintptr) *VesaFbConsole {
	return &VesaFbConsole{
		bpp:           uint32(bpp),
		bytesPerPixel: uint32(bpp+1) >> 3,
		fbPhysAddr:    fbPhysAddr,
		colorInfo:     colorInfo,
		width:         width,
		height:        height,
		pitch:         pitch,
		// light gray text on black background
		defaultFg: 7,
		defaultBg: 0,
		clearChar: uint16(' '),
	}
}

// SetFont selects a bitmap font to be used by the console.
func (cons *VesaFbConsole) SetFont(f *font.Font) {
	if f == nil {
		return
	}

	cons.font = f
	cons.widthInChars = cons.width / f.GlyphWidth
	cons.heightInChars = (cons.height - cons.offsetY) / f.GlyphHeight
}

// SetLogo selects the logo to be displayed by the console. The logo colors will
// be remapped to the end of the console's palette and space equal to the logo
// height will be reserved at the top of the framebuffer for diplaying the logo.
//
// As setting a logo changes the available space for rendering text, SetLogo
// must be invoked before SetFont.
func (cons *VesaFbConsole) SetLogo(l *logo.Image) {
	if l == nil {
		return
	}

	// Map the logo colors to the console palette replacing the transparent
	// color index with the console default bg color
	offset := uint8(len(cons.palette) - len(l.Palette))
	for i, rgba := range l.Palette {
		if uint8(i) == l.TransparentIndex {
			rgba = cons.palette[cons.defaultBg].(color.RGBA)
		}
		cons.setPaletteColor(uint8(i)+offset, rgba, false)
	}

	// Draw the logo
	var fbRowOffset uint32
	switch l.Align {
	case logo.AlignLeft:
		fbRowOffset = cons.fbOffset(0, 0)
	case logo.AlignCenter:
		fbRowOffset = cons.fbOffset((cons.width-l.Width)>>1, 0)
	case logo.AlignRight:
		fbRowOffset = cons.fbOffset(cons.width-l.Width, 0)
	}

	for y, logoOffset := uint32(0), 0; y < l.Height; y, fbRowOffset = y+1, fbRowOffset+cons.pitch {
		for x, fbOffset := uint32(0), fbRowOffset; x < l.Width; x, fbOffset, logoOffset = x+1, fbOffset+cons.bytesPerPixel, logoOffset+1 {
			c := l.Data[logoOffset] + offset

			switch cons.bpp {
			case 8:
				cons.fb[fbOffset] = c
			case 15, 16:
				colorComp := cons.packColor16(c)
				cons.fb[fbOffset] = colorComp[0]
				cons.fb[fbOffset+1] = colorComp[1]
			case 24, 32:
				colorComp := cons.packColor24(c)
				cons.fb[fbOffset] = colorComp[0]
				cons.fb[fbOffset+1] = colorComp[1]
				cons.fb[fbOffset+2] = colorComp[2]
			}
		}
	}

	cons.offsetY = l.Height
}

// Dimensions returns the console width and height in the specified dimension.
func (cons *VesaFbConsole) Dimensions(dim Dimension) (uint32, uint32) {
	switch dim {
	case Characters:
		return cons.widthInChars, cons.heightInChars
	default:
		return cons.width, cons.height
	}
}

// DefaultColors returns the default foreground and background colors
// used by this console.
func (cons *VesaFbConsole) DefaultColors() (fg uint8, bg uint8) {
	return cons.defaultFg, cons.defaultBg
}

// Fill sets the contents of the specified rectangular region to the requested
// color. Both x and y coordinates are 1-based.
func (cons *VesaFbConsole) Fill(x, y, width, height uint32, _, bg uint8) {
	if cons.font == nil {
		return
	}

	// clip rectangle
	if x == 0 {
		x = 1
	} else if x >= cons.widthInChars {
		x = cons.widthInChars
	}

	if y == 0 {
		y = 1
	} else if y >= cons.heightInChars {
		y = cons.heightInChars
	}

	if x+width-1 > cons.widthInChars {
		width = cons.widthInChars - x + 1
	}

	if y+height-1 > cons.heightInChars {
		height = cons.heightInChars - y + 1
	}

	pX := (x - 1) * cons.font.GlyphWidth
	pY := (y - 1) * cons.font.GlyphHeight
	pW := width * cons.font.GlyphWidth
	pH := height * cons.font.GlyphHeight
	switch cons.bpp {
	case 8:
		cons.fill8(pX, pY, pW, pH, bg)
	case 15, 16:
		cons.fill16(pX, pY, pW, pH, bg)
	case 24, 32:
		cons.fill24(pX, pY, pW, pH, bg)
	}
}

// fill8 implements a fill operation using an 8bpp framebuffer.
func (cons *VesaFbConsole) fill8(pX, pY, pW, pH uint32, bg uint8) {
	fbRowOffset := cons.fbOffset(pX, pY)
	for ; pH > 0; pH, fbRowOffset = pH-1, fbRowOffset+cons.pitch {
		for fbOffset := fbRowOffset; fbOffset < fbRowOffset+pW; fbOffset++ {
			cons.fb[fbOffset] = bg
		}
	}
}

// fill16 implements a fill operation using a 15/16bpp framebuffer.
func (cons *VesaFbConsole) fill16(pX, pY, pW, pH uint32, bg uint8) {
	comp := cons.packColor16(bg)
	fbRowOffset := cons.fbOffset(pX, pY)
	for ; pH > 0; pH, fbRowOffset = pH-1, fbRowOffset+cons.pitch {
		for fbOffset := fbRowOffset; fbOffset < fbRowOffset+pW*cons.bytesPerPixel; fbOffset += cons.bytesPerPixel {
			cons.fb[fbOffset] = comp[0]
			cons.fb[fbOffset+1] = comp[1]
		}
	}
}

// fill24 implements a fill operation using a 24/32bpp framebuffer.
func (cons *VesaFbConsole) fill24(pX, pY, pW, pH uint32, bg uint8) {
	comp := cons.packColor24(bg)
	fbRowOffset := cons.fbOffset(pX, pY)
	for ; pH > 0; pH, fbRowOffset = pH-1, fbRowOffset+cons.pitch {
		for fbOffset := fbRowOffset; fbOffset < fbRowOffset+pW*cons.bytesPerPixel; fbOffset += cons.bytesPerPixel {
			cons.fb[fbOffset] = comp[0]
			cons.fb[fbOffset+1] = comp[1]
			cons.fb[fbOffset+2] = comp[2]
		}
	}
}

// Scroll the console contents to the specified direction. The caller
// is responsible for updating (e.g. clear or replace) the contents of
// the region that was scrolled.
func (cons *VesaFbConsole) Scroll(dir ScrollDir, lines uint32) {
	if cons.font == nil || lines == 0 || lines > cons.heightInChars {
		return
	}

	offset := cons.fbOffset(0, lines*cons.font.GlyphHeight-cons.offsetY)

	switch dir {
	case ScrollDirUp:
		startOffset := cons.fbOffset(0, 0)
		endOffset := cons.fbOffset(0, cons.height-lines*cons.font.GlyphHeight-cons.offsetY)
		for i := startOffset; i < endOffset; i++ {
			cons.fb[i] = cons.fb[i+offset]
		}
	case ScrollDirDown:
		startOffset := cons.fbOffset(0, lines*cons.font.GlyphHeight)
		for i := uint32(len(cons.fb) - 1); i >= startOffset; i-- {
			cons.fb[i] = cons.fb[i-offset]
		}
	}
}

// Write a char to the specified location. If fg or bg exceed the supported
// colors for this console, they will be set to their default value. Both x and
// y coordinates are 1-based
func (cons *VesaFbConsole) Write(ch byte, fg, bg uint8, x, y uint32) {
	if x < 1 || x > cons.widthInChars || y < 1 || y > cons.heightInChars || cons.font == nil {
		return
	}

	pX := (x - 1) * cons.font.GlyphWidth
	pY := (y - 1) * cons.font.GlyphHeight

	switch cons.bpp {
	case 8:
		cons.write8(ch, fg, bg, pX, pY)
	case 15, 16:
		cons.write16(ch, fg, bg, pX, pY)
	case 24, 32:
		cons.write24(ch, fg, bg, pX, pY)
	}
}

// write8 writes a character using an 8bpp framebuffer.
func (cons *VesaFbConsole) write8(glyphIndex, fg, bg uint8, pX, pY uint32) {
	var (
		fontOffset  = uint32(glyphIndex) * cons.font.BytesPerRow * cons.font.GlyphHeight
		fbRowOffset = cons.fbOffset(pX, pY)
		fbOffset    uint32
		x, y        uint32
		mask        uint8
	)

	for y = 0; y < cons.font.GlyphHeight; y, fbRowOffset, fontOffset = y+1, fbRowOffset+cons.pitch, fontOffset+1 {
		fbOffset = fbRowOffset
		fontRowData := cons.font.Data[fontOffset]
		mask = 1 << 7
		for x = 0; x < cons.font.GlyphWidth; x, fbOffset, mask = x+1, fbOffset+1, mask>>1 {
			// If mask becomes zero while we are still in this loop
			// then the font uses > 1 byte per row. We need to
			// fetch the next byte and reset the mask.
			if mask == 0 {
				fontOffset++
				fontRowData = cons.font.Data[fontOffset]
				mask = 1 << 7
			}

			if (fontRowData & mask) != 0 {
				cons.fb[fbOffset] = fg
			} else {
				cons.fb[fbOffset] = bg
			}
		}
	}
}

// write16 writes a character using a 15/162bpp framebuffer.
func (cons *VesaFbConsole) write16(glyphIndex, fg, bg uint8, pX, pY uint32) {
	var (
		fontOffset  = uint32(glyphIndex) * cons.font.BytesPerRow * cons.font.GlyphHeight
		fbRowOffset = cons.fbOffset(pX, pY)
		fbOffset    uint32
		x, y        uint32
		mask        uint8
		fgComp      = cons.packColor16(fg)
		bgComp      = cons.packColor16(bg)
	)

	for y = 0; y < cons.font.GlyphHeight; y, fbRowOffset, fontOffset = y+1, fbRowOffset+cons.pitch, fontOffset+1 {
		fbOffset = fbRowOffset
		fontRowData := cons.font.Data[fontOffset]
		mask = 1 << 7
		for x = 0; x < cons.font.GlyphWidth; x, fbOffset, mask = x+1, fbOffset+cons.bytesPerPixel, mask>>1 {
			// If mask becomes zero while we are still in this loop
			// then the font uses > 1 byte per row. We need to
			// fetch the next byte and reset the mask.
			if mask == 0 {
				fontOffset++
				fontRowData = cons.font.Data[fontOffset]
				mask = 1 << 7
			}

			if (fontRowData & mask) != 0 {
				cons.fb[fbOffset] = fgComp[0]
				cons.fb[fbOffset+1] = fgComp[1]
			} else {
				cons.fb[fbOffset] = bgComp[0]
				cons.fb[fbOffset+1] = bgComp[1]
			}
		}
	}
}

// write24 writes a character using a 24/32bpp framebuffer.
func (cons *VesaFbConsole) write24(glyphIndex, fg, bg uint8, pX, pY uint32) {
	var (
		fontOffset  = uint32(glyphIndex) * cons.font.BytesPerRow * cons.font.GlyphHeight
		fbRowOffset = cons.fbOffset(pX, pY)
		fbOffset    uint32
		x, y        uint32
		mask        uint8
		fgComp      = cons.packColor24(fg)
		bgComp      = cons.packColor24(bg)
	)

	for y = 0; y < cons.font.GlyphHeight; y, fbRowOffset, fontOffset = y+1, fbRowOffset+cons.pitch, fontOffset+1 {
		fbOffset = fbRowOffset
		fontRowData := cons.font.Data[fontOffset]
		mask = 1 << 7
		for x = 0; x < cons.font.GlyphWidth; x, fbOffset, mask = x+1, fbOffset+cons.bytesPerPixel, mask>>1 {
			// If mask becomes zero while we are still in this loop
			// then the font uses > 1 byte per row. We need to
			// fetch the next byte and reset the mask.
			if mask == 0 {
				fontOffset++
				fontRowData = cons.font.Data[fontOffset]
				mask = 1 << 7
			}

			if (fontRowData & mask) != 0 {
				cons.fb[fbOffset] = fgComp[0]
				cons.fb[fbOffset+1] = fgComp[1]
				cons.fb[fbOffset+2] = fgComp[2]
			} else {
				cons.fb[fbOffset] = bgComp[0]
				cons.fb[fbOffset+1] = bgComp[1]
				cons.fb[fbOffset+2] = bgComp[2]
			}
		}
	}
}

// fbOffset returns the linear offset into the framebuffer that corresponds to
// the pixel at (x,y).
func (cons *VesaFbConsole) fbOffset(x, y uint32) uint32 {
	return ((y + cons.offsetY) * cons.pitch) + (x * cons.bytesPerPixel)
}

// packColor24 encodes a palette color into the pixel format required by a
// 24/32 bpp framebuffer.
func (cons *VesaFbConsole) packColor24(colorIndex uint8) [3]uint8 {
	var (
		c             = cons.palette[colorIndex].(color.RGBA)
		packed uint32 = 0 |
			(uint32(c.R>>(8-cons.colorInfo.RedMaskSize)) << cons.colorInfo.RedPosition) |
			(uint32(c.G>>(8-cons.colorInfo.GreenMaskSize)) << cons.colorInfo.GreenPosition) |
			(uint32(c.B>>(8-cons.colorInfo.BlueMaskSize)) << cons.colorInfo.BluePosition)
	)

	return [3]uint8{
		uint8(packed),
		uint8(packed >> 8),
		uint8(packed >> 16),
	}
}

// packColor16 encodes a palette color into the pixel format required by a
// 15/16 bpp framebuffer.
func (cons *VesaFbConsole) packColor16(colorIndex uint8) [2]uint8 {
	var (
		c             = cons.palette[colorIndex].(color.RGBA)
		packed uint16 = 0 |
			(uint16(c.R>>(8-cons.colorInfo.RedMaskSize)) << cons.colorInfo.RedPosition) |
			(uint16(c.G>>(8-cons.colorInfo.GreenMaskSize)) << cons.colorInfo.GreenPosition) |
			(uint16(c.B>>(8-cons.colorInfo.BlueMaskSize)) << cons.colorInfo.BluePosition)
	)

	return [2]uint8{
		uint8(packed),
		uint8(packed >> 8),
	}
}

// Palette returns the active color palette for this console.
func (cons *VesaFbConsole) Palette() color.Palette {
	return cons.palette
}

// SetPaletteColor updates the color definition for the specified
// palette index. Passing a color index greated than the number of
// supported colors should be a no-op.
func (cons *VesaFbConsole) SetPaletteColor(index uint8, rgba color.RGBA) {
	oldColor := cons.palette[index]

	if oldColor != nil && oldColor.(color.RGBA) == rgba {
		return
	}

	cons.setPaletteColor(index, rgba, true)
}

// setPaletteColor updates the color definition for the specified
// palette index. If replace is true, then all occurrences of the old color
// in the framebuffer will be replaced by the new color value (if bpp > 8).
func (cons *VesaFbConsole) setPaletteColor(index uint8, rgba color.RGBA, replace bool) {
	oldColor := cons.palette[index]
	cons.palette[index] = rgba

	switch cons.bpp {
	case 8:
		// Load palette entry to the DAC. Each DAC entry is a 6-bit value so
		// we need to scale the RGB values in the [0-63] range.
		portWriteByteFn(0x3c8, index)
		portWriteByteFn(0x3c9, rgba.R>>2)
		portWriteByteFn(0x3c9, rgba.G>>2)
		portWriteByteFn(0x3c9, rgba.B>>2)
	case 15, 16:
		if oldColor == nil || !replace {
			return
		}

		cons.replace16(oldColor.(color.RGBA), rgba)
	case 24, 32:
		if oldColor == nil || !replace {
			return
		}

		cons.replace24(oldColor.(color.RGBA), rgba)
	}
}

// replace16 replaces all srcColor values with dstColor using a 15/16bpp
// framebuffer.
func (cons *VesaFbConsole) replace16(src, dst color.RGBA) {
	tmp := cons.palette[0]
	cons.palette[0] = src
	srcComp := cons.packColor16(0)
	cons.palette[0] = dst
	dstComp := cons.packColor16(0)
	cons.palette[0] = tmp
	for fbOffset := cons.fbOffset(0, 0); fbOffset < uint32(len(cons.fb)); fbOffset += cons.bytesPerPixel {
		if cons.fb[fbOffset] == srcComp[0] &&
			cons.fb[fbOffset+1] == srcComp[1] {
			cons.fb[fbOffset] = dstComp[0]
			cons.fb[fbOffset+1] = dstComp[1]
		}
	}
}

// replace24 replaces all srcColor values with dstColor using a 24/32bpp
// framebuffer.
func (cons *VesaFbConsole) replace24(src, dst color.RGBA) {
	tmp := cons.palette[0]
	cons.palette[0] = src
	srcComp := cons.packColor24(0)
	cons.palette[0] = dst
	dstComp := cons.packColor24(0)
	cons.palette[0] = tmp
	for fbOffset := cons.fbOffset(0, 0); fbOffset < uint32(len(cons.fb)); fbOffset += cons.bytesPerPixel {
		if cons.fb[fbOffset] == srcComp[0] &&
			cons.fb[fbOffset+1] == srcComp[1] &&
			cons.fb[fbOffset+2] == srcComp[2] {
			cons.fb[fbOffset] = dstComp[0]
			cons.fb[fbOffset+1] = dstComp[1]
			cons.fb[fbOffset+2] = dstComp[2]
		}
	}
}

// loadDefaultPalette is called during driver initialization to setup the
// console palette. Regardless of the framebuffer depth, the console always
// uses a 256-color palette.
func (cons *VesaFbConsole) loadDefaultPalette() {
	cons.palette = make(color.Palette, 256)

	egaPalette := []color.RGBA{
		{R: 0, G: 0, B: 0},       /* black */
		{R: 0, G: 0, B: 128},     /* blue */
		{R: 0, G: 128, B: 1},     /* green */
		{R: 0, G: 128, B: 128},   /* cyan */
		{R: 128, G: 0, B: 1},     /* red */
		{R: 128, G: 0, B: 128},   /* magenta */
		{R: 64, G: 64, B: 1},     /* brown */
		{R: 128, G: 128, B: 128}, /* light gray */
		{R: 64, G: 64, B: 64},    /* dark gray */
		{R: 0, G: 0, B: 255},     /* light blue */
		{R: 0, G: 255, B: 1},     /* light green */
		{R: 0, G: 255, B: 255},   /* light cyan */
		{R: 255, G: 0, B: 1},     /* light red */
		{R: 255, G: 0, B: 255},   /* light magenta */
		{R: 255, G: 255, B: 1},   /* yellow */
		{R: 255, G: 255, B: 255}, /* white */
	}

	// Load default EGA palette for colors 0-16
	var index int
	for ; index < len(egaPalette); index++ {
		cons.SetPaletteColor(uint8(index), egaPalette[index])
	}

	// Set all other colors to black
	for ; index < len(cons.palette); index++ {
		cons.SetPaletteColor(uint8(index), egaPalette[0])
	}
}

// DriverName returns the name of this driver.
func (cons *VesaFbConsole) DriverName() string {
	return "vesa_fb_console"
}

// DriverVersion returns the version of this driver.
func (cons *VesaFbConsole) DriverVersion() (uint16, uint16, uint16) {
	return 0, 0, 1
}

// DriverInit initializes this driver.
func (cons *VesaFbConsole) DriverInit(w io.Writer) *kernel.Error {
	// Map the framebuffer so we can write to it
	fbSize := uintptr(cons.height * cons.pitch)
	fbPage, err := mapRegionFn(
		mm.Frame(cons.fbPhysAddr>>mm.PageShift),
		fbSize,
		vmm.FlagPresent|vmm.FlagRW,
	)

	if err != nil {
		return err
	}

	cons.fb = *(*[]uint8)(unsafe.Pointer(&reflect.SliceHeader{
		Len:  int(fbSize),
		Cap:  int(fbSize),
		Data: fbPage.Address(),
	}))

	kfmt.Fprintf(w, "mapped framebuffer to 0x%x\n", fbPage.Address())
	kfmt.Fprintf(w, "framebuffer dimensions: %dx%dx%d\n", cons.width, cons.height, cons.bpp)

	cons.loadDefaultPalette()

	return nil
}

// probeForVesaFbConsole checks for the presence of a vga text console.
func probeForVesaFbConsole() device.Driver {
	var drv device.Driver

	fbInfo := getFramebufferInfoFn()
	if fbInfo.Type == multiboot.FramebufferTypeIndexed || fbInfo.Type == multiboot.FramebufferTypeRGB {
		drv = NewVesaFbConsole(
			fbInfo.Width, fbInfo.Height,
			fbInfo.Bpp, fbInfo.Pitch,
			fbInfo.RGBColorInfo(),
			uintptr(fbInfo.PhysAddr),
		)
	}

	return drv
}

func init() {
	device.RegisterDriver(&device.DriverInfo{
		Order: device.DetectOrderEarly,
		Probe: probeForVesaFbConsole,
	})
}

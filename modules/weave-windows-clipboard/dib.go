//go:build windows

package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math/bits"
)

// Bitmap clipboard formats, from WinUser.h. Paint, the Snipping Tool and most
// older applications copy an image as these alone, with no PNG beside it, and
// paste only these: a PNG-only clipboard is no image to them.
const (
	cfBitmap = 2  // a device-dependent HBITMAP; Windows synthesizes CF_DIB from it
	cfDIB    = 8  // BITMAPINFO and its bits in one block: a packed DIB
	cfDIBV5  = 17 // the same with a BITMAPV5HEADER, which carries alpha
)

// BITMAPINFOHEADER.biCompression values, from wingdi.h.
const (
	biRGB            = 0
	biBitfields      = 3
	biPNG            = 5
	biAlphaBitfields = 6
)

const (
	infoHeaderSize = 40  // BITMAPINFOHEADER
	v5HeaderSize   = 124 // BITMAPV5HEADER
	// lcsSRGB ('sRGB') and lcsGMImages: the colour space and rendering
	// intent of a BITMAPV5HEADER, as Windows itself writes them.
	lcsSRGB     = 0x73524742
	lcsGMImages = 4
)

// maxBitmapPixels bounds a bitmap read or written: 64 Mpx is 256 MiB as
// 32-bit pixels, four times what a clipboard transfer may carry at all, so
// nothing a host can send or take is refused, and a header claiming more is
// a broken one rather than an allocation to attempt.
const maxBitmapPixels = 64 << 20

var errBadDIB = errors.New("not a bitmap this module reads")

// dibToPNG converts a packed DIB, as CF_DIB and CF_DIBV5 hold it, to PNG: the
// canonical image format a host takes.
func dibToPNG(dib []byte) ([]byte, error) {
	img, embedded, err := decodeDIB(dib)
	if err != nil {
		return nil, err
	}
	if embedded != nil {
		return embedded, nil
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, fmt.Errorf("encoding PNG: %w", err)
	}
	return out.Bytes(), nil
}

// dibHeader is what decodeDIB needs from a BITMAPINFOHEADER or one of its
// larger successors (V4, V5), which only append fields to it.
type dibHeader struct {
	size          int
	width, height int
	topDown       bool
	bitCount      int
	compression   uint32
	colorsUsed    int
	masks         [4]uint32 // red, green, blue, alpha
}

func le32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }

// decodeDIB reads a packed DIB: a header, the colour masks or colour table
// that follow it, then rows of pixels, each padded to four bytes, bottom row
// first unless the height is negative. A DIB whose pixels are a whole PNG
// (BI_PNG) is returned as that PNG.
func decodeDIB(b []byte) (image.Image, []byte, error) {
	h, off, err := parseDIBHeader(b)
	if err != nil {
		return nil, nil, err
	}
	if h.compression == biPNG {
		if !bytes.HasPrefix(b[off:], []byte("\x89PNG\r\n\x1a\n")) {
			return nil, nil, fmt.Errorf("%w: BI_PNG without a PNG", errBadDIB)
		}
		return nil, b[off:], nil
	}

	var palette []color.NRGBA
	if h.bitCount <= 8 {
		n := h.colorsUsed
		if n == 0 || n > 1<<h.bitCount {
			n = 1 << h.bitCount
		}
		if len(b) < off+4*n {
			return nil, nil, fmt.Errorf("%w: its colour table is cut short", errBadDIB)
		}
		for i := range n {
			q := b[off+4*i:] // RGBQUAD: blue, green, red, reserved
			palette = append(palette, color.NRGBA{R: q[2], G: q[1], B: q[0], A: 0xff})
		}
		off += 4 * n
	} else {
		// A colour table beside true-colour pixels only optimises display
		// on a palette device; the pixels come after it.
		off += 4 * h.colorsUsed
	}

	stride := (h.width*h.bitCount + 31) / 32 * 4
	need := stride * h.height
	if off > len(b) || len(b)-off < need {
		return nil, nil, fmt.Errorf(
			"%w: %d bytes of pixels, want %d",
			errBadDIB,
			len(b)-min(off, len(b)),
			need,
		)
	}
	// Some writers repeat a V4 or V5 header's masks after it, as a
	// BITMAPINFOHEADER's would be; the pixels then end exactly 12 bytes later.
	if h.size > infoHeaderSize && h.compression != biRGB && len(b)-off == need+12 {
		off += 12
	}

	img := image.NewNRGBA(image.Rect(0, 0, h.width, h.height))
	alpha := false
	for y := range h.height {
		src := h.height - 1 - y
		if h.topDown {
			src = y
		}
		row := b[off+src*stride : off+src*stride+stride]
		dst := img.Pix[y*img.Stride:]
		for x := range h.width {
			c := h.pixel(row, x, palette)
			dst[4*x], dst[4*x+1], dst[4*x+2], dst[4*x+3] = c.R, c.G, c.B, c.A
			alpha = alpha || c.A != 0
		}
	}
	// A 32-bit bitmap's fourth byte is alpha to some writers and padding,
	// left zero, to the rest. Every pixel transparent is the second: an image
	// nobody could see is not what anyone copied.
	if !alpha {
		for i := 3; i < len(img.Pix); i += 4 {
			img.Pix[i] = 0xff
		}
	}
	return img, nil, nil
}

// parseDIBHeader reads the header and returns it with the offset just past
// it and any masks that follow it.
func parseDIBHeader(b []byte) (dibHeader, int, error) {
	var h dibHeader
	if len(b) < infoHeaderSize {
		return h, 0, fmt.Errorf("%w: %d bytes is shorter than its header", errBadDIB, len(b))
	}
	size := le32(b)
	if size < infoHeaderSize || int64(size) > int64(len(b)) {
		// BITMAPCOREHEADER (12 bytes) is OS/2's, and no clipboard holds it.
		return h, 0, fmt.Errorf("%w: a %d-byte header", errBadDIB, size)
	}
	h.size = int(size)
	w := int64(int32(le32(b[4:])))  //nolint:gosec // G115: biWidth is a LONG
	ht := int64(int32(le32(b[8:]))) //nolint:gosec // G115: biHeight is a LONG
	h.topDown = ht < 0
	if h.topDown {
		ht = -ht
	}
	if w <= 0 || ht <= 0 || w*ht > maxBitmapPixels {
		return h, 0, fmt.Errorf("%w: %d by %d pixels", errBadDIB, w, ht)
	}
	h.width, h.height = int(w), int(ht)
	h.bitCount = int(binary.LittleEndian.Uint16(b[14:]))
	h.compression = le32(b[16:])
	h.colorsUsed = int(min(le32(b[32:]), 256))
	off := h.size

	switch h.compression {
	case biPNG:
		return h, off, nil
	case biRGB:
		switch h.bitCount {
		case 1, 4, 8, 24:
		case 16:
			h.masks = [4]uint32{0x7c00, 0x03e0, 0x001f, 0} // 5-5-5
		case 32:
			h.masks = [4]uint32{0xff0000, 0xff00, 0xff, 0xff000000}
		default:
			return h, 0, fmt.Errorf("%w: %d bits per pixel", errBadDIB, h.bitCount)
		}
	case biBitfields, biAlphaBitfields:
		if h.bitCount != 16 && h.bitCount != 32 {
			return h, 0, fmt.Errorf("%w: bit fields at %d bits per pixel", errBadDIB, h.bitCount)
		}
		n := 3
		if h.compression == biAlphaBitfields {
			n = 4
		}
		// A BITMAPINFOHEADER is followed by the masks; a V2 header or
		// larger holds them, and V3 and up the alpha mask too.
		at := infoHeaderSize
		switch {
		case h.size == infoHeaderSize:
			if len(b) < off+4*n {
				return h, 0, fmt.Errorf("%w: its colour masks are cut short", errBadDIB)
			}
			off += 4 * n
		case h.size >= infoHeaderSize+16:
			n = 4
		case h.size < infoHeaderSize+12:
			return h, 0, fmt.Errorf("%w: a %d-byte header with bit fields", errBadDIB, h.size)
		}
		for i := range n {
			h.masks[i] = le32(b[at+4*i:])
		}
	default:
		return h, 0, fmt.Errorf("%w: compression %d", errBadDIB, h.compression)
	}
	return h, off, nil
}

// pixel reads pixel x of a row.
func (h dibHeader) pixel(row []byte, x int, palette []color.NRGBA) color.NRGBA {
	switch h.bitCount {
	case 1, 4, 8:
		per := 8 / h.bitCount
		shift := 8 - h.bitCount*(x%per+1)
		i := int(row[x/per]>>shift) & (1<<h.bitCount - 1)
		if i >= len(palette) {
			return color.NRGBA{A: 0xff}
		}
		return palette[i]
	case 24:
		return color.NRGBA{R: row[3*x+2], G: row[3*x+1], B: row[3*x], A: 0xff}
	case 16:
		v := uint32(binary.LittleEndian.Uint16(row[2*x:]))
		return h.fromMasks(v)
	default: // 32
		return h.fromMasks(le32(row[4*x:]))
	}
}

// fromMasks scales each masked field of v to 8 bits. Without an alpha mask
// the pixel is opaque; decodeDIB treats an image whose every alpha is zero
// as opaque as well.
func (h dibHeader) fromMasks(v uint32) color.NRGBA {
	c := color.NRGBA{
		R: field(v, h.masks[0]), G: field(v, h.masks[1]), B: field(v, h.masks[2]), A: 0xff,
	}
	if h.masks[3] != 0 {
		c.A = field(v, h.masks[3])
	}
	return c
}

// field is v's bits under mask, scaled to 0-255.
func field(v, mask uint32) uint8 {
	if mask == 0 {
		return 0
	}
	shift := bits.TrailingZeros32(mask)
	top := mask >> shift
	return uint8(uint64(v&mask>>shift) * 255 / uint64(top)) //nolint:gosec // G115: at most 255
}

// pngToDIBs converts a PNG to the two bitmap formats applications paste: a
// CF_DIBV5 with straight alpha, and a 24-bit CF_DIB, composited over white,
// for applications that read only that and would show transparency as
// black. Both are bottom-up, as Windows' own are.
func pngToDIBs(data []byte) (dib, dibv5 []byte, err error) {
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, nil, fmt.Errorf("reading PNG: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxBitmapPixels {
		return nil, nil, fmt.Errorf("%w: %d by %d pixels", errBadDIB, cfg.Width, cfg.Height)
	}
	src, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, nil, fmt.Errorf("decoding PNG: %w", err)
	}
	w, h := cfg.Width, cfg.Height
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), src, src.Bounds().Min, draw.Src)

	stride24 := (w*3 + 3) &^ 3
	dib = bitmapInfoHeader(infoHeaderSize, w, h, 24, biRGB, stride24*h)
	dibv5 = bitmapInfoHeader(v5HeaderSize, w, h, 32, biBitfields, 4*w*h)
	for _, m := range []uint32{0xff0000, 0xff00, 0xff, 0xff000000, lcsSRGB} {
		dibv5 = binary.LittleEndian.AppendUint32(dibv5, m)
	}
	dibv5 = append(dibv5, make([]byte, 36+12)...) // CIEXYZTRIPLE endpoints, gamma: unused for sRGB
	dibv5 = binary.LittleEndian.AppendUint32(dibv5, lcsGMImages)
	dibv5 = append(dibv5, make([]byte, 12)...) // profile data, profile size, reserved

	pad := make([]byte, stride24-w*3)
	for y := h - 1; y >= 0; y-- {
		row := img.Pix[y*img.Stride:]
		for x := range w {
			r, g, b, a := row[4*x], row[4*x+1], row[4*x+2], row[4*x+3]
			dibv5 = append(dibv5, b, g, r, a)
			dib = append(dib, overWhite(b, a), overWhite(g, a), overWhite(r, a))
		}
		dib = append(dib, pad...)
	}
	return dib, dibv5, nil
}

// bitmapInfoHeader appends a BITMAPINFOHEADER's fields, the first 40 bytes of
// every header size, for a bottom-up bitmap.
func bitmapInfoHeader(size, w, h, bitCount int, compression uint32, image int) []byte {
	le := binary.LittleEndian
	out := le.AppendUint32(nil, uint32(size))    //nolint:gosec // G115: a header size
	out = le.AppendUint32(out, uint32(w))        //nolint:gosec // G115: bounded by maxBitmapPixels
	out = le.AppendUint32(out, uint32(h))        //nolint:gosec // G115: bounded by maxBitmapPixels
	out = le.AppendUint16(out, 1)                // planes
	out = le.AppendUint16(out, uint16(bitCount)) //nolint:gosec // G115: 24 or 32
	out = le.AppendUint32(out, compression)
	out = le.AppendUint32(out, uint32(image)) //nolint:gosec // G115: bounded by maxBitmapPixels
	// Resolution (96 dpi, as pixels per metre), colours used and important.
	out = le.AppendUint32(out, 3780)
	out = le.AppendUint32(out, 3780)
	return append(out, make([]byte, 8)...)
}

// overWhite composites one straight-alpha channel over white.
func overWhite(c, a uint8) uint8 {
	v := (uint32(c)*uint32(a) + 255*(255-uint32(a)) + 127) / 255
	return uint8(v) //nolint:gosec // G115: at most 255
}

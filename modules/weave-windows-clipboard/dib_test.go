//go:build windows

package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// The fixtures are built field by field from the layouts in wingdi.h, so each
// one is the bitmap its name says: header size, bit depth, compression, row
// order and padding.

// dibFixture is one bitmap: its header fields, what follows the header
// (masks, a colour table) and its rows, top row first; rows are padded and
// ordered by build.
type dibFixture struct {
	header      int // 40, 52, 56, 108 or 124
	width       int
	height      int
	topDown     bool
	bitCount    int
	compression uint32
	colorsUsed  int
	masks       []uint32 // in the header when header > 40, after it when 40
	trailing    []byte   // after the header (and masks): a colour table, repeated masks
	rows        [][]byte // unpadded, top first
}

func (f dibFixture) build() []byte {
	le := binary.LittleEndian
	h := int32(f.height) //nolint:gosec // test sizes
	if f.topDown {
		h = -h
	}
	b := le.AppendUint32(nil, uint32(f.header))    //nolint:gosec // test sizes
	b = le.AppendUint32(b, uint32(int32(f.width))) //nolint:gosec // test sizes
	b = le.AppendUint32(b, uint32(h))              //nolint:gosec // test sizes
	b = le.AppendUint16(b, 1)                      // planes
	b = le.AppendUint16(b, uint16(f.bitCount))     //nolint:gosec // test sizes
	b = le.AppendUint32(b, f.compression)          // compression
	b = le.AppendUint32(b, 0)                      // image size: 0 is allowed for BI_RGB
	b = le.AppendUint32(b, 0)                      // x pixels per metre
	b = le.AppendUint32(b, 0)                      // y pixels per metre
	b = le.AppendUint32(b, uint32(f.colorsUsed))   //nolint:gosec // test sizes
	b = le.AppendUint32(b, 0)                      // important colours
	for _, m := range f.masks {
		b = le.AppendUint32(b, m)
	}
	for len(b) < f.header {
		b = append(b, 0)
	}
	b = append(b, f.trailing...)
	stride := (f.width*f.bitCount + 31) / 32 * 4
	order := make([][]byte, len(f.rows))
	for i, r := range f.rows {
		if f.topDown {
			order[i] = r
		} else {
			order[len(f.rows)-1-i] = r
		}
	}
	for _, r := range order {
		row := make([]byte, stride)
		copy(row, r)
		b = append(b, row...)
	}
	return b
}

// pixels decodes PNG data to straight-alpha pixels, top row first.
func pixels(t *testing.T, data []byte) [][]color.NRGBA {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("not a PNG: %v", err)
	}
	b := img.Bounds()
	out := make([][]color.NRGBA, b.Dy())
	for y := range out {
		for x := range b.Dx() {
			c, _ := color.NRGBAModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA)
			out[y] = append(out[y], c)
		}
	}
	return out
}

func rgb(r, g, b uint8) color.NRGBA     { return color.NRGBA{R: r, G: g, B: b, A: 0xff} }
func rgba(r, g, b, a uint8) color.NRGBA { return color.NRGBA{R: r, G: g, B: b, A: a} }

func samePixels(t *testing.T, name string, got, want [][]color.NRGBA) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d rows, want %d", name, len(got), len(want))
	}
	for y := range want {
		if len(got[y]) != len(want[y]) {
			t.Fatalf("%s: row %d is %d wide, want %d", name, y, len(got[y]), len(want[y]))
		}
		for x := range want[y] {
			if got[y][x] != want[y][x] {
				t.Errorf("%s: pixel (%d,%d) is %v, want %v", name, x, y, got[y][x], want[y][x])
			}
		}
	}
}

func TestDIBToPNG(t *testing.T) {
	bgra := func(cs ...color.NRGBA) []byte {
		var out []byte
		for _, c := range cs {
			out = append(out, c.B, c.G, c.R, c.A)
		}
		return out
	}
	bgr := func(cs ...color.NRGBA) []byte {
		var out []byte
		for _, c := range cs {
			out = append(out, c.B, c.G, c.R)
		}
		return out
	}
	le16 := func(vs ...uint16) []byte {
		var out []byte
		for _, v := range vs {
			out = binary.LittleEndian.AppendUint16(out, v)
		}
		return out
	}
	quads := func(cs ...color.NRGBA) []byte { // RGBQUADs, reserved byte zero
		var out []byte
		for _, c := range cs {
			out = append(out, c.B, c.G, c.R, 0)
		}
		return out
	}
	red, green, blue, white := rgb(255, 0, 0), rgb(0, 255, 0), rgb(0, 0, 255), rgb(255, 255, 255)
	half := rgba(10, 20, 30, 128)
	clear := rgba(1, 2, 3, 0)
	v5masks := []uint32{0xff0000, 0xff00, 0xff, 0xff000000, lcsSRGB}

	for name, tc := range map[string]struct {
		dib  dibFixture
		want [][]color.NRGBA
	}{
		// Three 24-bit pixels are nine bytes, padded to twelve per row.
		"24-bit, odd width, bottom-up": {
			dibFixture{
				header: 40, width: 3, height: 2, bitCount: 24,
				rows: [][]byte{bgr(red, green, blue), bgr(white, blue, red)},
			},
			[][]color.NRGBA{{red, green, blue}, {white, blue, red}},
		},
		"24-bit, top-down": {
			dibFixture{
				header: 40, width: 1, height: 3, topDown: true, bitCount: 24,
				rows: [][]byte{bgr(red), bgr(green), bgr(blue)},
			},
			[][]color.NRGBA{{red}, {green}, {blue}},
		},
		"32-bit with alpha": {
			dibFixture{
				header: 40, width: 2, height: 2, bitCount: 32,
				rows: [][]byte{bgra(half, clear), bgra(red, rgba(0, 0, 255, 200))},
			},
			[][]color.NRGBA{{half, clear}, {red, rgba(0, 0, 255, 200)}},
		},
		// The fourth byte is padding to most writers: all zero is opaque.
		"32-bit, alpha all zero": {
			dibFixture{
				header: 40, width: 2, height: 1, bitCount: 32,
				rows: [][]byte{bgra(rgba(255, 0, 0, 0), rgba(0, 0, 255, 0))},
			},
			[][]color.NRGBA{{red, blue}},
		},
		"BI_BITFIELDS, 32-bit, masks after the header": {
			dibFixture{
				header: 40, width: 2, height: 1, bitCount: 32, compression: biBitfields,
				trailing: binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint32(
					binary.LittleEndian.AppendUint32(nil, 0xff0000), 0xff00), 0xff),
				rows: [][]byte{bgra(rgba(255, 0, 0, 0), rgba(0, 255, 0, 0))},
			},
			[][]color.NRGBA{{red, green}},
		},
		"BI_BITFIELDS, 16-bit 5-6-5, odd width": {
			dibFixture{
				header: 40, width: 3, height: 1, bitCount: 16, compression: biBitfields,
				trailing: binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint32(
					binary.LittleEndian.AppendUint32(nil, 0xf800), 0x07e0), 0x001f),
				rows: [][]byte{le16(0xf800, 0x07e0, 0x001f)},
			},
			[][]color.NRGBA{{red, green, blue}},
		},
		"BI_ALPHABITFIELDS, masks after the header": {
			dibFixture{
				header: 40, width: 1, height: 1, bitCount: 32, compression: biAlphaBitfields,
				trailing: binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint32(
					binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint32(nil,
						0xff), 0xff00), 0xff0000), 0xff000000), // red and blue swapped
				rows: [][]byte{{10, 20, 30, 128}},
			},
			[][]color.NRGBA{{rgba(10, 20, 30, 128)}},
		},
		"16-bit BI_RGB is 5-5-5": {
			dibFixture{
				header: 40, width: 2, height: 1, bitCount: 16,
				rows: [][]byte{le16(0x7c00, 0x001f)},
			},
			[][]color.NRGBA{{red, blue}},
		},
		"V5, BI_BITFIELDS with alpha, top-down": {
			dibFixture{
				header: v5HeaderSize, width: 2, height: 2, topDown: true, bitCount: 32,
				compression: biBitfields, masks: v5masks,
				rows: [][]byte{bgra(half, red), bgra(clear, white)},
			},
			[][]color.NRGBA{{half, red}, {clear, white}},
		},
		"V5, masks repeated after the header": {
			dibFixture{
				header: v5HeaderSize, width: 1, height: 1, bitCount: 32,
				compression: biBitfields, masks: v5masks,
				trailing: make([]byte, 12),
				rows:     [][]byte{bgra(half)},
			},
			[][]color.NRGBA{{half}},
		},
		"V5, BI_RGB": {
			dibFixture{
				header: v5HeaderSize, width: 1, height: 1, bitCount: 24,
				rows: [][]byte{bgr(green)},
			},
			[][]color.NRGBA{{green}},
		},
		"V2 header, three masks": {
			dibFixture{
				header: 52, width: 1, height: 1, bitCount: 32, compression: biBitfields,
				masks: []uint32{0xff0000, 0xff00, 0xff},
				rows:  [][]byte{bgra(rgba(0, 0, 255, 0))},
			},
			[][]color.NRGBA{{blue}},
		},
		"8-bit palette": {
			dibFixture{
				header: 40, width: 3, height: 1, bitCount: 8, colorsUsed: 3,
				trailing: quads(red, green, blue),
				rows:     [][]byte{{2, 1, 0}},
			},
			[][]color.NRGBA{{blue, green, red}},
		},
		"4-bit palette, odd width": {
			dibFixture{
				header: 40, width: 3, height: 1, bitCount: 4, colorsUsed: 2,
				trailing: quads(white, red),
				rows:     [][]byte{{0x10, 0x10}},
			},
			[][]color.NRGBA{{red, white, red}},
		},
		// An index past a short colour table reads as black.
		"1-bit, nine wide, index past the table": {
			dibFixture{
				header: 40, width: 9, height: 1, bitCount: 1, colorsUsed: 1,
				trailing: quads(white),
				rows:     [][]byte{{0b0111_1111, 0b1000_0000}},
			},
			[][]color.NRGBA{{
				white, rgb(0, 0, 0), rgb(0, 0, 0), rgb(0, 0, 0), rgb(0, 0, 0),
				rgb(0, 0, 0), rgb(0, 0, 0), rgb(0, 0, 0), rgb(0, 0, 0),
			}},
		},
		"true colour after a colour table": {
			dibFixture{
				header: 40, width: 1, height: 1, bitCount: 24, colorsUsed: 2,
				trailing: quads(white, white),
				rows:     [][]byte{bgr(red)},
			},
			[][]color.NRGBA{{red}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := dibToPNG(tc.dib.build())
			if err != nil {
				t.Fatal(err)
			}
			samePixels(t, name, pixels(t, out), tc.want)
		})
	}
}

func TestDIBWithAPNGInside(t *testing.T) {
	inner := encodePNG(t, image.NewNRGBA(image.Rect(0, 0, 2, 2)))
	d := dibFixture{header: 40, width: 2, height: 2, bitCount: 0, compression: biPNG}.build()
	got, err := dibToPNG(append(d, inner...))
	if err != nil || !bytes.Equal(got, inner) {
		t.Fatalf("BI_PNG: %d bytes, %v; want the PNG inside as it is", len(got), err)
	}
	if _, err := dibToPNG(append(d, "GIF89a"...)); !errors.Is(err, errBadDIB) {
		t.Errorf("BI_PNG without a PNG: %v", err)
	}
}

func TestDIBsThatAreRefused(t *testing.T) {
	ok := dibFixture{header: 40, width: 2, height: 2, bitCount: 24, rows: [][]byte{{}, {}}}
	header := func(edit func([]byte)) []byte {
		b := ok.build()
		edit(b)
		return b
	}
	for name, b := range map[string][]byte{
		"shorter than a header": make([]byte, 39),
		"core header":           header(func(b []byte) { binary.LittleEndian.PutUint32(b, 12) }),
		"header past the end":   header(func(b []byte) { binary.LittleEndian.PutUint32(b, 4096) }),
		"no width":              header(func(b []byte) { binary.LittleEndian.PutUint32(b[4:], 0) }),
		"no height":             header(func(b []byte) { binary.LittleEndian.PutUint32(b[8:], 0) }),
		"too many pixels": header(func(b []byte) {
			binary.LittleEndian.PutUint32(b[4:], 1<<14)
			binary.LittleEndian.PutUint32(b[8:], 1<<14)
		}),
		"rows cut short":   ok.build()[:50],
		"2 bits per pixel": header(func(b []byte) { binary.LittleEndian.PutUint16(b[14:], 2) }),
		"run-length coded": header(func(b []byte) { binary.LittleEndian.PutUint32(b[16:], 1) }),
		"JPEG":             header(func(b []byte) { binary.LittleEndian.PutUint32(b[16:], 4) }),
		"bit fields at 24": header(func(b []byte) { binary.LittleEndian.PutUint32(b[16:], biBitfields) }),
		"masks cut short": dibFixture{
			header: 40, width: 1, height: 1, bitCount: 32, compression: biBitfields, rows: [][]byte{{}},
		}.build(),
		"colour table cut":   dibFixture{header: 40, width: 1, height: 1, bitCount: 8, rows: [][]byte{{}}}.build(),
		"44-byte bit fields": dibFixture{header: 44, width: 1, height: 1, bitCount: 32, compression: biBitfields, rows: [][]byte{{}}}.build(),
	} {
		if _, err := dibToPNG(b); !errors.Is(err, errBadDIB) {
			t.Errorf("%s: %v, want errBadDIB", name, err)
		}
	}
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// A PNG from the host becomes a CF_DIBV5 that keeps its alpha and a 24-bit
// CF_DIB composited over white, both of which read back to the pixels they
// were made from.
func TestPNGToDIBs(t *testing.T) {
	// Five wide: fifteen bytes a row at 24 bits, padded to sixteen.
	img := image.NewNRGBA(image.Rect(0, 0, 5, 2))
	want := [][]color.NRGBA{
		{
			rgb(255, 0, 0),
			rgba(0, 255, 0, 128),
			rgba(0, 0, 255, 0),
			rgb(1, 2, 3),
			rgba(200, 100, 50, 255),
		},
		{rgba(0, 0, 0, 64), rgb(9, 9, 9), rgb(255, 255, 255), rgba(40, 50, 60, 70), rgb(0, 0, 0)},
	}
	for y, row := range want {
		for x, c := range row {
			img.SetNRGBA(x, y, c)
		}
	}
	dib, dibv5, err := pngToDIBs(encodePNG(t, img))
	if err != nil {
		t.Fatal(err)
	}
	if n := binary.LittleEndian.Uint32(
		dibv5,
	); n != v5HeaderSize ||
		len(dibv5) != v5HeaderSize+4*5*2 {
		t.Errorf("CF_DIBV5: a %d-byte header in %d bytes", n, len(dibv5))
	}
	if n := binary.LittleEndian.Uint32(
		dib,
	); n != infoHeaderSize ||
		len(dib) != infoHeaderSize+16*2 {
		t.Errorf("CF_DIB: a %d-byte header in %d bytes", n, len(dib))
	}
	if h := int32(binary.LittleEndian.Uint32(dibv5[8:])); h != 2 { //nolint:gosec // a test height
		t.Errorf("CF_DIBV5 height %d, want 2: bottom-up, as Windows writes it", h)
	}

	v5, err := dibToPNG(dibv5)
	if err != nil {
		t.Fatal(err)
	}
	samePixels(t, "CF_DIBV5", pixels(t, v5), want)

	flat, err := dibToPNG(dib)
	if err != nil {
		t.Fatal(err)
	}
	overWhiteWant := make([][]color.NRGBA, len(want))
	for y, row := range want {
		for _, c := range row {
			overWhiteWant[y] = append(overWhiteWant[y],
				rgb(overWhite(c.R, c.A), overWhite(c.G, c.A), overWhite(c.B, c.A)))
		}
	}
	samePixels(t, "CF_DIB", pixels(t, flat), overWhiteWant)
	if got := overWhite(0, 128); got != 127 {
		t.Errorf("black at half alpha over white is %d, want 127", got)
	}

	// A paletted or premultiplied source converts the same way.
	pal := image.NewPaletted(image.Rect(0, 0, 1, 1), color.Palette{rgb(4, 5, 6)})
	if _, v5, err := pngToDIBs(encodePNG(t, pal)); err != nil || len(v5) != v5HeaderSize+4 {
		t.Errorf("paletted PNG: %d bytes, %v", len(v5), err)
	}
}

// pngHeader is a PNG signature and IHDR claiming width by height, and
// nothing after it.
func pngHeader(width, height uint32) []byte {
	ihdr := binary.BigEndian.AppendUint32([]byte("IHDR"), width)
	ihdr = binary.BigEndian.AppendUint32(ihdr, height)
	ihdr = append(ihdr, 8, 6, 0, 0, 0) // 8-bit RGBA
	b := append([]byte("\x89PNG\r\n\x1a\n"), 0, 0, 0, 13)
	b = append(b, ihdr...)
	return binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(ihdr))
}

func TestPNGsThatMakeNoBitmap(t *testing.T) {
	for name, data := range map[string][]byte{
		"not a PNG":       []byte("\x89PNG and then nothing"),
		"too many pixels": pngHeader(1<<14, 1<<14),
		"no image data":   pngHeader(2, 2),
	} {
		if _, _, err := pngToDIBs(data); err == nil {
			t.Errorf("%s: converted", name)
		}
	}
}

package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
)

// IconDir represents the ICO file header
type IconDir struct {
	Reserved uint16
	Type     uint16
	Count    uint16
}

// IconDirEntry represents each image entry inside ICO
type IconDirEntry struct {
	Width       uint8
	Height      uint8
	ColorCount  uint8
	Reserved    uint8
	Planes      uint16
	BitCount    uint16
	BytesInRes  uint32
	ImageOffset uint32
}

func loadSource(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return nil, err
	}
	return img, nil
}

// resizeBox downscales with area averaging so 16px tray icons stay readable.
func resizeBox(src image.Image, size int) *image.RGBA {
	sb := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	if sb.Dx() == size && sb.Dy() == size {
		draw.Draw(dst, dst.Bounds(), src, sb.Min, draw.Src)
		return dst
	}

	scaleX := float64(sb.Dx()) / float64(size)
	scaleY := float64(sb.Dy()) / float64(size)
	for y := 0; y < size; y++ {
		y0 := sb.Min.Y + int(float64(y)*scaleY)
		y1 := sb.Min.Y + int(float64(y+1)*scaleY)
		if y1 <= y0 {
			y1 = y0 + 1
		}
		if y1 > sb.Max.Y {
			y1 = sb.Max.Y
		}
		for x := 0; x < size; x++ {
			x0 := sb.Min.X + int(float64(x)*scaleX)
			x1 := sb.Min.X + int(float64(x+1)*scaleX)
			if x1 <= x0 {
				x1 = x0 + 1
			}
			if x1 > sb.Max.X {
				x1 = sb.Max.X
			}

			var r, g, b, a uint32
			var n uint32
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					pr, pg, pb, pa := src.At(sx, sy).RGBA()
					r += pr
					g += pg
					b += pb
					a += pa
					n++
				}
			}
			if n == 0 {
				continue
			}
			dst.Pix[dst.PixOffset(x, y)+0] = uint8((r / n) >> 8)
			dst.Pix[dst.PixOffset(x, y)+1] = uint8((g / n) >> 8)
			dst.Pix[dst.PixOffset(x, y)+2] = uint8((b / n) >> 8)
			dst.Pix[dst.PixOffset(x, y)+3] = uint8((a / n) >> 8)
		}
	}
	return dst
}

func encodeICODIB(img *image.RGBA) []byte {
	size := img.Bounds().Dx()
	xorStride := size * 4
	andStride := (size + 31) / 32 * 4
	headerSize := 40
	buf := make([]byte, headerSize+xorStride*size+andStride*size)

	binary.LittleEndian.PutUint32(buf[0:4], 40)
	binary.LittleEndian.PutUint32(buf[4:8], uint32(size))
	binary.LittleEndian.PutUint32(buf[8:12], uint32(size*2))
	binary.LittleEndian.PutUint16(buf[12:14], 1)
	binary.LittleEndian.PutUint16(buf[14:16], 32)
	binary.LittleEndian.PutUint32(buf[20:24], uint32(xorStride*size+andStride*size))

	// ICO XOR bitmap is bottom-up BGRA. AND mask stays zero so the
	// per-pixel alpha in the XOR layer is what Windows actually uses.
	for y := 0; y < size; y++ {
		src := img.Pix[y*img.Stride : y*img.Stride+xorStride]
		dst := buf[headerSize+(size-1-y)*xorStride:]
		for x := 0; x < size; x++ {
			dst[x*4+0] = src[x*4+2]
			dst[x*4+1] = src[x*4+1]
			dst[x*4+2] = src[x*4+0]
			dst[x*4+3] = src[x*4+3]
		}
	}
	return buf
}

func isGlyphPixel(c color.Color) bool {
	r, g, b, a := c.RGBA()
	if a < 0x2000 {
		return false
	}
	return (r*299+g*587+b*114)/1000 > 0x1800
}

func cropGlyph(src image.Image) image.Image {
	sb := src.Bounds()
	minX, minY := sb.Max.X, sb.Max.Y
	maxX, maxY := sb.Min.X, sb.Min.Y
	for y := sb.Min.Y; y < sb.Max.Y; y++ {
		for x := sb.Min.X; x < sb.Max.X; x++ {
			if !isGlyphPixel(src.At(x, y)) {
				continue
			}
			if x < minX {
				minX = x
			}
			if y < minY {
				minY = y
			}
			if x >= maxX {
				maxX = x + 1
			}
			if y >= maxY {
				maxY = y + 1
			}
		}
	}
	if minX >= maxX || minY >= maxY {
		return src
	}

	w, h := maxX-minX, maxY-minY
	side := w
	if h > side {
		side = h
	}
	pad := side / 10
	if pad < 1 {
		pad = 1
	}
	side += pad * 2
	cx := (minX + maxX) / 2
	cy := (minY + maxY) / 2
	x0 := cx - side/2
	y0 := cy - side/2
	if x0 < sb.Min.X {
		x0 = sb.Min.X
	}
	if y0 < sb.Min.Y {
		y0 = sb.Min.Y
	}
	if x0+side > sb.Max.X {
		x0 = sb.Max.X - side
	}
	if y0+side > sb.Max.Y {
		y0 = sb.Max.Y - side
	}
	if x0 < sb.Min.X {
		x0 = sb.Min.X
	}
	if y0 < sb.Min.Y {
		y0 = sb.Min.Y
	}
	crop := image.Rect(x0, y0, x0+side, y0+side).Intersect(sb)
	if crop.Empty() {
		return src
	}
	out := image.NewRGBA(image.Rect(0, 0, crop.Dx(), crop.Dy()))
	draw.Draw(out, out.Bounds(), src, crop.Min, draw.Src)
	return out
}

func renderIconFrame(src image.Image, size int) *image.RGBA {
	// Tray and title-bar slots are 16–48px. Tight-crop the monogram so
	// the black poster margin does not shrink the glyph to a few pixels.
	if size <= 48 {
		src = cropGlyph(src)
	}
	return resizeBox(src, size)
}

func encodeIconImage(src image.Image, size int) ([]byte, error) {
	img := renderIconFrame(src, size)
	// LoadImage / Shell_NotifyIcon still mishandle PNG-compressed frames
	// below 256px and then fall back to a stale %TEMP% copy of the old icon.
	if size < 256 {
		return encodeICODIB(img), nil
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func buildICO(src image.Image, sizes []int) ([]byte, error) {
	images := make([][]byte, 0, len(sizes))
	for _, size := range sizes {
		payload, err := encodeIconImage(src, size)
		if err != nil {
			return nil, err
		}
		images = append(images, payload)
	}

	icoBuf := new(bytes.Buffer)
	header := IconDir{Reserved: 0, Type: 1, Count: uint16(len(sizes))}
	if err := binary.Write(icoBuf, binary.LittleEndian, header); err != nil {
		return nil, err
	}

	offset := uint32(6 + 16*len(sizes))
	for i, size := range sizes {
		var w, h uint8
		if size >= 256 {
			w, h = 0, 0
		} else {
			w, h = uint8(size), uint8(size)
		}
		entry := IconDirEntry{
			Width:       w,
			Height:      h,
			ColorCount:  0,
			Reserved:    0,
			Planes:      1,
			BitCount:    32,
			BytesInRes:  uint32(len(images[i])),
			ImageOffset: offset,
		}
		if err := binary.Write(icoBuf, binary.LittleEndian, entry); err != nil {
			return nil, err
		}
		offset += uint32(len(images[i]))
	}
	for _, payload := range images {
		if _, err := icoBuf.Write(payload); err != nil {
			return nil, err
		}
	}
	return icoBuf.Bytes(), nil
}

func main() {
	source := "assets/icon.png"
	if env := os.Getenv("RDPULSE_ICON_PNG"); env != "" {
		source = env
	}
	src, err := loadSource(source)
	if err != nil {
		panic(fmt.Errorf("read %s: %w", source, err))
	}

	sizes := []int{16, 24, 32, 48, 64, 128, 256}
	ico, err := buildICO(src, sizes)
	if err != nil {
		panic(err)
	}

	targets := []string{"assets/icon.ico", "internal/gui/assets/icon.ico"}
	for _, target := range targets {
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			panic(err)
		}
		if err := os.WriteFile(target, ico, 0644); err != nil {
			panic(err)
		}
	}
	fmt.Printf("Generated icon.ico from %s (%d bytes, %d sizes)\n", source, len(ico), len(sizes))
}

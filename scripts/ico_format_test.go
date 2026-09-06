package main

import (
	"encoding/binary"
	"image"
	"image/color"
	"testing"
)

func TestTrayIconFrameFillsGlyph(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 22; y < 42; y++ {
		for x := 22; x < 42; x++ {
			src.SetRGBA(x, y, color.RGBA{R: 0, G: 180, B: 255, A: 255})
		}
	}

	frame := renderIconFrame(src, 16)
	var lit int
	for i := 0; i < len(frame.Pix); i += 4 {
		if int(frame.Pix[i])+int(frame.Pix[i+1])+int(frame.Pix[i+2]) > 40 {
			lit++
		}
	}
	if lit < 16*16*45/100 {
		t.Fatalf("tray 16px glyph covers %d/256 pixels, want at least 45%% — source padding is eating the slot", lit)
	}

	large := renderIconFrame(src, 256)
	if large.Bounds().Dx() != 256 {
		t.Fatalf("large frame size = %d", large.Bounds().Dx())
	}
}

func TestRealLogoTrayFrameFillsSlot(t *testing.T) {
	src, err := loadSource("../assets/icon.png")
	if err != nil {
		t.Skip(err)
	}
	frame := renderIconFrame(src, 16)
	var lit int
	for i := 0; i < len(frame.Pix); i += 4 {
		if int(frame.Pix[i])+int(frame.Pix[i+1])+int(frame.Pix[i+2]) > 40 {
			lit++
		}
	}
	if lit < 16*16*35/100 {
		t.Fatalf("real logo 16px covers %d/256 pixels, still too padded", lit)
	}
}

func TestBuildICOUsesBitmapForTitleBarSizes(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			src.SetRGBA(x, y, color.RGBA{R: 0, G: 180, B: 255, A: 255})
		}
	}

	ico, err := buildICO(src, []int{16, 32, 256})
	if err != nil {
		t.Fatal(err)
	}
	if len(ico) < 6 {
		t.Fatalf("ico too small: %d", len(ico))
	}
	count := binary.LittleEndian.Uint16(ico[4:6])
	if count != 3 {
		t.Fatalf("count = %d, want 3", count)
	}

	for i, size := range []int{16, 32, 256} {
		entry := 6 + 16*i
		bytesInRes := binary.LittleEndian.Uint32(ico[entry+8 : entry+12])
		offset := binary.LittleEndian.Uint32(ico[entry+12 : entry+16])
		if int(offset+8) > len(ico) {
			t.Fatalf("size %d: offset %d out of range", size, offset)
		}
		payload := ico[offset : offset+bytesInRes]
		isPNG := len(payload) >= 8 && string(payload[:8]) == "\x89PNG\r\n\x1a\n"
		isBMP := len(payload) >= 4 && binary.LittleEndian.Uint32(payload[:4]) == 40

		if size < 256 {
			if !isBMP {
				t.Fatalf("size %d must be a 32-bit DIB for LoadImage (tray/title bar), got png=%v", size, isPNG)
			}
		} else if !isPNG {
			t.Fatalf("size 256 should stay PNG, bmp=%v", isBMP)
		}
	}
}

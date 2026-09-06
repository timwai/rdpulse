package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
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

func dist(x1, y1, x2, y2 float64) float64 {
	return math.Hypot(x1-x2, y1-y2)
}

// distToSegment returns the distance from point (px, py) to line segment (x1, y1)-(x2, y2)
func distToSegment(px, py, x1, y1, x2, y2 float64) float64 {
	dx := x2 - x1
	dy := y2 - y1
	if dx == 0 && dy == 0 {
		return math.Hypot(px-x1, py-y1)
	}
	t := ((px-x1)*dx + (py-y1)*dy) / (dx*dx + dy*dy)
	if t < 0 {
		return math.Hypot(px-x1, py-y1)
	} else if t > 1 {
		return math.Hypot(px-x2, py-y2)
	}
	projX := x1 + t*dx
	projY := y1 + t*dy
	return math.Hypot(px-projX, py-projY)
}

// renderIcon renders Concept A (Desktop Pulse) at a given size
func renderIcon(size int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	s := float64(size)

	var (
		cr, pad, x0, y0, x1, y1 float64
		monX0, monY0, monW, monH, monCR, monStroke, pulseStroke float64
	)

	isTray := size <= 32

	if isTray {
		// Tray micro-optimization: The monitor fills the canvas for maximum visibility
		pad = 0.5
		x0, y0 = pad, pad
		x1, y1 = s - pad, s - pad
		cr = s * 0.18

		monX0 = s * 0.08
		monY0 = s * 0.08
		monW = s * 0.84
		monH = s * 0.62
		monCR = s * 0.12
		monStroke = math.Max(1.5, s*0.09)
		pulseStroke = math.Max(1.6, s*0.11)
	} else {
		// Full badge (48, 64, 128, 256): Luxury squircle container with glassmorphic monitor
		cr = s * 0.22
		pad = s * 0.04
		x0, y0 = pad, pad
		x1, y1 = s - pad, s - pad

		monX0 = s * 0.16
		monY0 = s * 0.18
		monW = s * 0.68
		monH = s * 0.48
		monCR = s * 0.06
		monStroke = math.Max(1.2, s*0.048)
		pulseStroke = math.Max(1.4, s*0.062)
	}

	type Point struct {
		x, y float64
	}

	pulsePts := []Point{
		{0.08, 0.48},
		{0.30, 0.48},
		{0.38, 0.32},
		{0.48, 0.68},
		{0.55, 0.40},
		{0.62, 0.53},
		{0.67, 0.48},
		{0.92, 0.48},
	}

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			px := float64(x) + 0.5
			py := float64(y) + 0.5

			// 1. Calculate distance to squircle
			cx := math.Max(x0+cr, math.Min(px, x1-cr))
			cy := math.Max(y0+cr, math.Min(py, y1-cr))
			dSquircle := dist(px, py, cx, cy) - cr

			// Antialiasing for squircle edge
			var squircleAlpha float64
			if dSquircle <= -0.5 {
				squircleAlpha = 1.0
			} else if dSquircle >= 0.5 {
				squircleAlpha = 0.0
			} else {
				squircleAlpha = 0.5 - dSquircle
			}

			if squircleAlpha <= 0 {
				continue
			}

			// Background gradient: Deep Midnight Navy (#080F21) to Rich Cobalt/Slate (#0F1E3A)
			gradT := py / s
			bgR := uint8(8 + gradT*12)
			bgG := uint8(14 + gradT*18)
			bgB := uint8(32 + gradT*30)

			// Subtle border highlight on squircle
			dBorder := math.Abs(dSquircle)
			if dBorder < s*0.025 {
				borderBlend := 1.0 - (dBorder / (s * 0.025))
				bgR = uint8(float64(bgR)*(1-borderBlend*0.5) + 56*borderBlend*0.5)
				bgG = uint8(float64(bgG)*(1-borderBlend*0.5) + 189*borderBlend*0.5)
				bgB = uint8(float64(bgB)*(1-borderBlend*0.5) + 248*borderBlend*0.5)
			}

			colR := float64(bgR)
			colG := float64(bgG)
			colB := float64(bgB)

			// 2. Monitor screen outline
			mcx := math.Max(monX0+monCR, math.Min(px, monX0+monW-monCR))
			mcy := math.Max(monY0+monCR, math.Min(py, monY0+monH-monCR))
			dMon := dist(px, py, mcx, mcy) - monCR
			dMonStroke := math.Abs(dMon)

			if dMonStroke <= monStroke/2+0.6 {
				alphaMon := 1.0
				if dMonStroke > monStroke/2-0.6 {
					alphaMon = (monStroke/2 + 0.6 - dMonStroke) / 1.2
				}
				// Sky blue monitor frame (#38BDF8)
				colR = colR*(1-alphaMon) + 56*alphaMon
				colG = colG*(1-alphaMon) + 189*alphaMon
				colB = colB*(1-alphaMon) + 248*alphaMon
			}

			// Monitor stand and base
			standDist := distToSegment(px, py, s*0.5, monY0+monH, s*0.5, s*0.77)
			if standDist <= monStroke/2+0.5 {
				alphaStand := 1.0
				if standDist > monStroke/2-0.5 {
					alphaStand = (monStroke/2 + 0.5 - standDist)
				}
				colR = colR*(1-alphaStand) + 14*alphaStand
				colG = colG*(1-alphaStand) + 165*alphaStand
				colB = colB*(1-alphaStand) + 233*alphaStand
			}

			baseDist := distToSegment(px, py, s*0.35, s*0.77, s*0.65, s*0.77)
			if baseDist <= monStroke/2+0.5 {
				alphaBase := 1.0
				if baseDist > monStroke/2-0.5 {
					alphaBase = (monStroke/2 + 0.5 - baseDist)
				}
				colR = colR*(1-alphaBase) + 56*alphaBase
				colG = colG*(1-alphaBase) + 189*alphaBase
				colB = colB*(1-alphaBase) + 248*alphaBase
			}

			// 3. Pulse line (The Heartbeat of RDPulse)
			minPulseDist := 999.0
			for i := 0; i < len(pulsePts)-1; i++ {
				dSeg := distToSegment(px, py, pulsePts[i].x*s, pulsePts[i].y*s, pulsePts[i+1].x*s, pulsePts[i+1].y*s)
				if dSeg < minPulseDist {
					minPulseDist = dSeg
				}
			}

			// Outer Glow for pulse line
			glowRadius := pulseStroke * 2.2
			if minPulseDist < glowRadius {
				glowAlpha := (1.0 - (minPulseDist / glowRadius)) * 0.45
				colR = colR*(1-glowAlpha) + 6*glowAlpha
				colG = colG*(1-glowAlpha) + 182*glowAlpha
				colB = colB*(1-glowAlpha) + 212*glowAlpha
			}

			// Core bright pulse line (Electric Cyan #00F2FE to Neon Emerald #10B981)
			if minPulseDist <= pulseStroke/2+0.6 {
				alphaPulse := 1.0
				if minPulseDist > pulseStroke/2-0.6 {
					alphaPulse = (pulseStroke/2 + 0.6 - minPulseDist) / 1.2
				}
				ptT := (px / s)
				pR := 6.0*(1-ptT) + 34.0*ptT
				pG := 220.0*(1-ptT) + 211.0*ptT
				pB := 240.0*(1-ptT) + 238.0*ptT

				colR = colR*(1-alphaPulse) + pR*alphaPulse
				colG = colG*(1-alphaPulse) + pG*alphaPulse
				colB = colB*(1-alphaPulse) + pB*alphaPulse
			}

			img.SetRGBA(x, y, color.RGBA{
				R: uint8(math.Min(255, colR)),
				G: uint8(math.Min(255, colG)),
				B: uint8(math.Min(255, colB)),
				A: uint8(squircleAlpha * 255),
			})
		}
	}
	return img
}

func main() {
	sizes := []int{16, 24, 32, 48, 64, 128, 256}
	var pngBuffers [][]byte

	for _, size := range sizes {
		img := renderIcon(size)
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			panic(err)
		}
		pngBuffers = append(pngBuffers, buf.Bytes())
	}

	// Build .ICO file
	icoBuf := new(bytes.Buffer)

	// Write ICONDIR
	header := IconDir{
		Reserved: 0,
		Type:     1, // 1 = ICO
		Count:    uint16(len(sizes)),
	}
	binary.Write(icoBuf, binary.LittleEndian, header)

	// Offset starts right after header and all entries
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
			BytesInRes:  uint32(len(pngBuffers[i])),
			ImageOffset: offset,
		}
		binary.Write(icoBuf, binary.LittleEndian, entry)
		offset += uint32(len(pngBuffers[i]))
	}

	// Write PNG images payload
	for _, pngData := range pngBuffers {
		icoBuf.Write(pngData)
	}

	targets := []string{"assets/icon.ico", "internal/gui/assets/icon.ico"}
	for _, target := range targets {
		dir := "assets"
		if target != "assets/icon.ico" {
			dir = "internal/gui/assets"
		}
		_ = os.MkdirAll(dir, 0755)
		if err := os.WriteFile(target, icoBuf.Bytes(), 0644); err != nil {
			panic(err)
		}
	}
	fmt.Printf("Successfully generated icon.ico (%d bytes, %d mipmap sizes)\n", icoBuf.Len(), len(sizes))
}

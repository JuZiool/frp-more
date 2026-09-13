// genicon renders the FRP-More app icon (blue rounded square + white "F+")
// as PNGs for the fnOS package. Stdlib only.
//
// Usage: go run ./fnos/genicon -out <dir>   → writes icon_64.png / icon_256.png
package main

import (
	"flag"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

var blue = color.RGBA{0x25, 0x63, 0xeb, 0xff} // --accent
var white = color.RGBA{0xff, 0xff, 0xff, 0xff}

func main() {
	out := flag.String("out", ".", "output directory")
	flag.Parse()

	for _, size := range []int{64, 256} {
		img := render(size)
		path := filepath.Join(*out, "icon_"+itoa(size)+".png")
		f, err := os.Create(path)
		if err != nil {
			panic(err)
		}
		if err := png.Encode(f, img); err != nil {
			panic(err)
		}
		f.Close()
		println("written", path)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// render draws a size×size icon: rounded square background plus an "F+"
// monogram built from crisp rectangles (independent of font availability).
func render(size int) image.Image {
	s := float64(size) / 256.0 // scale relative to the 256 design grid
	img := image.NewRGBA(image.Rect(0, 0, size, size))

	// Rounded-square background with 2x supersampled edges.
	r := 56 * s
	half := 128 * s
	cx, cy := half, half
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			if roundedRectHit(float64(px)+0.5, float64(py)+0.5, cx, cy, half, half, r) {
				img.Set(px, py, blue)
			}
		}
	}

	// "F" monogram.
	fillRect(img, 72*s, 72*s, 100*s, 184*s)   // F vertical bar
	fillRect(img, 72*s, 72*s, 156*s, 100*s)   // F top bar
	fillRect(img, 72*s, 128*s, 142*s, 156*s)  // F middle bar
	// "+" monogram.
	fillRect(img, 158*s, 96*s, 186*s, 184*s)  // + vertical bar
	fillRect(img, 128*s, 126*s, 216*s, 154*s) // + horizontal bar

	return img
}

func roundedRectHit(px, py, cx, cy, halfW, halfH, r float64) bool {
	qx := math.Abs(px-cx) - (halfW - r)
	qy := math.Abs(py-cy) - (halfH - r)
	dx := math.Max(qx, 0)
	dy := math.Max(qy, 0)
	return math.Hypot(dx, dy)+math.Min(math.Max(qx, qy), 0)-r <= 0
}

func fillRect(img *image.RGBA, x0, y0, x1, y1 float64) {
	for py := int(math.Ceil(y0)); py < int(y1); py++ {
		for px := int(math.Ceil(x0)); px < int(x1); px++ {
			if image.Pt(px, py).In(img.Bounds()) {
				img.Set(px, py, white)
			}
		}
	}
}

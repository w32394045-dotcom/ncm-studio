// Generates the application icon as a PNG, so the release pipeline does not
// depend on an image tool being installed and the icon is reproducible from
// source rather than a binary blob nobody can edit.
//
// Usage: go run ./packaging/icon <out.png> [size]
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"strconv"
)

func main() {
	out := "icon.png"
	if len(os.Args) > 1 {
		out = os.Args[1]
	}
	size := 256
	if len(os.Args) > 2 {
		if n, err := strconv.Atoi(os.Args[2]); err == nil && n > 0 {
			size = n
		}
	}

	img := draw(size)
	if err := writePNG(out, img); err != nil {
		fmt.Fprintln(os.Stderr, "icon:", err)
		os.Exit(1)
	}
	fmt.Printf("%s: %dx%d\n", out, size, size)
}

// The mark: the app's blue, rounded, with a white quaver. Geometry is in
// fractions of the size, so the same code draws 32 px and 512 px.
func draw(size int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	bg := color.RGBA{0x1E, 0x5C, 0xD8, 0xFF}
	fg := color.RGBA{0xFF, 0xFF, 0xFF, 0xFF}

	// A rounded square, the shape every launcher grid expects.
	const radius = 0.22
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			fx := (float64(x) + 0.5) / float64(size)
			fy := (float64(y) + 0.5) / float64(size)
			if insideRounded(fx, fy, radius) {
				img.SetRGBA(x, y, bg)
			}
		}
	}

	// The note: a stem on the right, a beam at the top, and a filled head at
	// the bottom left. Drawn as shapes rather than text so no font is needed.
	stemX, stemW := 0.62, 0.075
	stemTop, stemBottom := 0.24, 0.70
	beamH := 0.085
	headCX, headCY, headRX, headRY := 0.46, 0.70, 0.155, 0.115

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			fx := (float64(x) + 0.5) / float64(size)
			fy := (float64(y) + 0.5) / float64(size)

			// Stem.
			if fx >= stemX && fx <= stemX+stemW && fy >= stemTop && fy <= stemBottom {
				img.SetRGBA(x, y, fg)
				continue
			}
			// Beam, slanting up to the right.
			if fy >= stemTop && fy <= stemTop+beamH && fx >= 0.33 && fx <= stemX+stemW {
				img.SetRGBA(x, y, fg)
				continue
			}
			// Head, as an ellipse.
			dx := (fx - headCX) / headRX
			dy := (fy - headCY) / headRY
			if dx*dx+dy*dy <= 1 {
				img.SetRGBA(x, y, fg)
			}
		}
	}
	return img
}

// insideRounded reports whether a point is inside a square with rounded
// corners, both in 0..1 coordinates.
func insideRounded(x, y, r float64) bool {
	if x < 0 || x > 1 || y < 0 || y > 1 {
		return false
	}
	cx := math.Min(math.Max(x, r), 1-r)
	cy := math.Min(math.Max(y, r), 1-r)
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= r*r
}

// writePNG encodes the image with the standard library and writes it out.
func writePNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

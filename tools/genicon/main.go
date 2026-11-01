// Command genicon renders the application icon (a white Fluent cloud on a
// rounded Windows accent blue square) into the PNG files that go-winres
// embeds in the executable and release.sh packs into the installer icon.
//
// Run from the repository root:
//
//	go run ./tools/genicon
package main

import (
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log"
	"math"
	"os"

	"github.com/mevdschee/ssh-cloud/internal/icons"
)

var background = color.RGBA{0x00, 0x78, 0xd4, 0xff} // Windows accent blue

func main() {
	for _, out := range []struct {
		file string
		size int
	}{
		{"winres/icon.png", 256},
		{"winres/icon48.png", 48},
		{"winres/icon32.png", 32},
		{"winres/icon16.png", 16},
	} {
		if err := write(out.file, render(out.size)); err != nil {
			log.Fatal(err)
		}
	}
}

// render draws the icon at one size: a rounded square (corner radius 22% of
// the size, supersampled for smooth edges) with the cloud at 70% of the size.
func render(size int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	const ss = 4 // supersampling per axis
	r := 0.22 * float64(size)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			hits := 0
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					if inRoundedRect(float64(x)+(float64(sx)+0.5)/ss, float64(y)+(float64(sy)+0.5)/ss, float64(size), r) {
						hits++
					}
				}
			}
			a := hits * 255 / (ss * ss)
			img.SetRGBA(x, y, color.RGBA{ // premultiplied
				uint8(int(background.R) * a / 255), uint8(int(background.G) * a / 255),
				uint8(int(background.B) * a / 255), uint8(a),
			})
		}
	}
	cloudSize := int(math.Round(0.7 * float64(size)))
	cloud, err := icons.Render("cloud_filled", cloudSize, color.RGBA{0xff, 0xff, 0xff, 0xff})
	if err != nil {
		log.Fatal(err)
	}
	off := (size - cloudSize) / 2
	draw.Draw(img, image.Rect(off, off, off+cloudSize, off+cloudSize), cloud, image.Point{}, draw.Over)
	return img
}

func inRoundedRect(x, y, size, r float64) bool {
	cx := math.Max(r, math.Min(x, size-r))
	cy := math.Max(r, math.Min(y, size-r))
	return (x-cx)*(x-cx)+(y-cy)*(y-cy) <= r*r
}

func write(file string, img image.Image) error {
	f, err := os.Create(file)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

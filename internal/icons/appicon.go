package icons

import (
	"image"
	"image/color"
	"image/draw"
	"math"
)

// Background is the app icon's colour, the Windows accent blue.
var Background = color.RGBA{0x00, 0x78, 0xd4, 0xff}

// AppIcon renders the application icon at an exact size: a white Fluent
// cloud on a rounded accent blue square (corner radius 22% of the size,
// supersampled for smooth edges). Rendering at the exact size keeps it sharp,
// unlike scaling one of the icon resource's fixed sizes.
func AppIcon(size int) *image.RGBA {
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
				uint8(int(Background.R) * a / 255), uint8(int(Background.G) * a / 255),
				uint8(int(Background.B) * a / 255), uint8(a),
			})
		}
	}
	cloudSize := int(math.Round(0.7 * float64(size)))
	if cloud, err := Render("cloud_filled", cloudSize, color.RGBA{0xff, 0xff, 0xff, 0xff}); err == nil {
		off := (size - cloudSize) / 2
		draw.Draw(img, image.Rect(off, off, off+cloudSize, off+cloudSize), cloud, image.Point{}, draw.Over)
	}
	return img
}

func inRoundedRect(x, y, size, r float64) bool {
	cx := math.Max(r, math.Min(x, size-r))
	cy := math.Max(r, math.Min(y, size-r))
	return (x-cx)*(x-cx)+(y-cy)*(y-cy) <= r*r
}

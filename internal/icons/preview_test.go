package icons

import (
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"testing"
)

// TestPreview writes a contact sheet of all icons when ICON_PREVIEW is set.
func TestPreview(t *testing.T) {
	out := os.Getenv("ICON_PREVIEW")
	if out == "" {
		t.Skip("set ICON_PREVIEW=file.png")
	}
	names := Names()
	const size, pad = 40, 12
	sheet := image.NewRGBA(image.Rect(0, 0, len(names)*(size+pad)+pad, size+2*pad))
	draw.Draw(sheet, sheet.Bounds(), image.White, image.Point{}, draw.Src)
	for i, n := range names {
		img, err := Render(n, size, color.RGBA{0x24, 0x24, 0x24, 0xff})
		if err != nil {
			t.Fatal(err)
		}
		x := pad + i*(size+pad)
		draw.Draw(sheet, image.Rect(x, pad, x+size, pad+size), img, image.Point{}, draw.Over)
	}
	f, _ := os.Create(out)
	defer f.Close()
	png.Encode(f, sheet)
}

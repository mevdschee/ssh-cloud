package icons

import (
	"image/color"
	"testing"
)

func TestAllIconsRender(t *testing.T) {
	names := Names()
	if len(names) < 16 {
		t.Fatalf("expected the toolbar icons, got %v", names)
	}
	for _, size := range []int{20, 24, 30, 40} { // never below the 20px design size
		for _, name := range names {
			img, err := Render(name, size, color.RGBA{0x20, 0x40, 0x60, 0xff})
			if err != nil {
				t.Fatal(err)
			}
			visible, transparent := 0, 0
			for i := 3; i < len(img.Pix); i += 4 {
				switch a := img.Pix[i]; {
				case a == 0:
					transparent++
				case a >= 0xa0:
					visible++
				}
			}
			if visible == 0 || transparent == 0 {
				t.Fatalf("%s at %dpx: %d visible, %d transparent pixels", name, size, visible, transparent)
			}
		}
	}
}

func TestColour(t *testing.T) {
	img, err := Render("arrow_left", 40, color.RGBA{0xff, 0, 0, 0xff})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(img.Pix); i += 4 {
		if img.Pix[i+3] == 0xff {
			if img.Pix[i] != 0xff || img.Pix[i+1] != 0 || img.Pix[i+2] != 0 {
				t.Fatalf("pixel %v is not the requested red", img.Pix[i:i+4])
			}
			return
		}
	}
	t.Fatal("no opaque pixel")
}

func TestUnknownIcon(t *testing.T) {
	if _, err := Render("nope", 20, color.RGBA{A: 0xff}); err == nil {
		t.Fatal("unknown icon must fail")
	}
}

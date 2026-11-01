// Command genicon renders the application icon (a white Fluent cloud on a
// rounded Windows accent blue square, see icons.AppIcon) into the PNG files
// that go-winres embeds in the executable and release.sh packs into the
// installer icon.
//
// Run from the repository root:
//
//	go run ./tools/genicon
package main

import (
	"image"
	"image/png"
	"log"
	"os"

	"github.com/mevdschee/ssh-cloud/internal/icons"
)

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
		if err := write(out.file, icons.AppIcon(out.size)); err != nil {
			log.Fatal(err)
		}
	}
}

func write(file string, img image.Image) error {
	f, err := os.Create(file)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

// Package icons renders the Fluent UI System Icons used in the toolbar
// (github.com/microsoft/fluentui-system-icons, MIT licence, see svg/LICENSE).
// The 20px "regular" SVGs are embedded and rasterised at runtime at the size
// the screen's DPI needs, in any colour.
package icons

import (
	"bytes"
	"embed"
	"fmt"
	"image"
	"image/color"
	"path"
	"sort"
	"strings"

	"github.com/srwiley/oksvg"
	"github.com/srwiley/rasterx"
)

//go:embed svg/*.svg
var files embed.FS

// The colour the Fluent SVGs are drawn in; replaced by the requested colour.
const sourceFill = "#212121"

// Names lists the available icons.
func Names() []string {
	entries, _ := files.ReadDir("svg")
	var names []string
	for _, e := range entries {
		names = append(names, strings.TrimSuffix(e.Name(), ".svg"))
	}
	sort.Strings(names)
	return names
}

// Render rasterises an icon to a size x size image with premultiplied alpha
// (image.RGBA), drawn in colour c.
func Render(name string, size int, c color.RGBA) (*image.RGBA, error) {
	data, err := files.ReadFile(path.Join("svg", name+".svg"))
	if err != nil {
		return nil, fmt.Errorf("icon %q: %w", name, err)
	}
	hex := fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B)
	data = bytes.ReplaceAll(data, []byte(sourceFill), []byte(hex))
	icon, err := oksvg.ReadIconStream(bytes.NewReader(data), oksvg.StrictErrorMode)
	if err != nil {
		return nil, fmt.Errorf("icon %q: %w", name, err)
	}
	icon.SetTarget(0, 0, float64(size), float64(size))
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	scanner := rasterx.NewScannerGV(size, size, img, img.Bounds())
	icon.Draw(rasterx.NewDasher(size, size, scanner), float64(c.A)/255)
	return img, nil
}

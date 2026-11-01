// Command mkico packs PNG images into a Windows .ico file (PNG compressed
// entries, supported since Windows Vista). It is used by release.sh to make
// the installer icon from the PNGs in winres.
//
//	go run ./tools/mkico out.ico in1.png in2.png ...
package main

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"log"
	"os"
)

func main() {
	if len(os.Args) < 3 {
		log.Fatal("usage: mkico out.ico in.png...")
	}
	var images [][]byte
	var sizes [][2]int
	for _, name := range os.Args[2:] {
		data, err := os.ReadFile(name)
		if err != nil {
			log.Fatal(err)
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			log.Fatalf("%s: %v", name, err)
		}
		images = append(images, data)
		sizes = append(sizes, [2]int{cfg.Width, cfg.Height})
	}

	var buf bytes.Buffer
	le := func(v any) { binary.Write(&buf, binary.LittleEndian, v) }
	le(uint16(0))           // reserved
	le(uint16(1))           // type: icon
	le(uint16(len(images))) // count
	offset := 6 + 16*len(images)
	for i, data := range images {
		dim := func(v int) uint8 { // 0 means 256 or more
			if v >= 256 {
				return 0
			}
			return uint8(v)
		}
		le(dim(sizes[i][0]))
		le(dim(sizes[i][1]))
		le(uint8(0))  // palette colors
		le(uint8(0))  // reserved
		le(uint16(1)) // color planes
		le(uint16(32))
		le(uint32(len(data)))
		le(uint32(offset))
		offset += len(data)
	}
	for _, data := range images {
		buf.Write(data)
	}
	if err := os.WriteFile(os.Args[1], buf.Bytes(), 0o644); err != nil {
		log.Fatal(err)
	}
}

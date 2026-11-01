package dragdata

import (
	"encoding/binary"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func TestFileGroupDescriptorLayout(t *testing.T) {
	mod := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	items := []Item{
		{Name: "proj", IsDir: true},
		{Name: `proj\a.txt`, Size: 5<<32 + 7, ModTime: mod},
	}
	buf, err := FileGroupDescriptor(items)
	if err != nil {
		t.Fatal(err)
	}
	if len(buf) != 4+2*592 {
		t.Fatalf("size %d", len(buf))
	}
	if n := binary.LittleEndian.Uint32(buf); n != 2 {
		t.Fatalf("count %d", n)
	}

	dir := buf[4 : 4+592]
	if attrs := binary.LittleEndian.Uint32(dir[36:]); attrs != 0x10 {
		t.Fatalf("folder attributes %#x", attrs)
	}
	if flags := binary.LittleEndian.Uint32(dir[0:]); flags&fdFileSize != 0 || flags&fdUnicode == 0 {
		t.Fatalf("folder flags %#x", flags)
	}

	file := buf[4+592:]
	flags := binary.LittleEndian.Uint32(file[0:])
	if flags&(fdAttributes|fdFileSize|fdWritesTime|fdUnicode) != fdAttributes|fdFileSize|fdWritesTime|fdUnicode {
		t.Fatalf("file flags %#x", flags)
	}
	if hi, lo := binary.LittleEndian.Uint32(file[64:]), binary.LittleEndian.Uint32(file[68:]); hi != 5 || lo != 7 {
		t.Fatalf("size high=%d low=%d", hi, lo)
	}
	// 2026-11-01T12:00:00Z as FILETIME.
	if ft := binary.LittleEndian.Uint64(file[56:]); ft != 134380080000000000 {
		t.Fatalf("filetime %d", ft)
	}
	name := make([]uint16, 0, 16)
	for i := 72; ; i += 2 {
		c := binary.LittleEndian.Uint16(file[i:])
		if c == 0 {
			break
		}
		name = append(name, c)
	}
	if got := string(utf16.Decode(name)); got != `proj\a.txt` {
		t.Fatalf("name %q", got)
	}
}

func TestTooLongNameRefused(t *testing.T) {
	if _, err := FileGroupDescriptor([]Item{{Name: strings.Repeat("a", 260)}}); err == nil {
		t.Fatal("names of 260+ characters do not fit in cFileName")
	}
	if _, err := FileGroupDescriptor([]Item{{Name: strings.Repeat("a", 259)}}); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsName(t *testing.T) {
	if got := WindowsName("a/b/c.txt"); got != `a\b\c.txt` {
		t.Fatal(got)
	}
}

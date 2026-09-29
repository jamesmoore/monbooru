package metadata

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// TIFF 6.0 field type codes.
const (
	exifByte      uint16 = 1
	exifASCII     uint16 = 2
	exifShort     uint16 = 3
	exifLong      uint16 = 4
	exifRational  uint16 = 5
	exifSByte     uint16 = 6
	exifUndefined uint16 = 7
	exifSShort    uint16 = 8
	exifSLong     uint16 = 9
	exifSRational uint16 = 10
	exifFloat     uint16 = 11
	exifDouble    uint16 = 12
)

var exifTypeSize = map[uint16]uint64{
	exifByte: 1, exifASCII: 1, exifShort: 2, exifLong: 4,
	exifRational: 8, exifSByte: 1, exifUndefined: 1, exifSShort: 2,
	exifSLong: 4, exifSRational: 8, exifFloat: 4, exifDouble: 8,
}

// Looked up by the names the tag tables give them.
const (
	exifIFDPointer    = "ExifIFDPointer"
	gpsIFDPointer     = "GPSInfoIFDPointer"
	interopIFDPointer = "InteroperabilityIFDPointer"
	userCommentField  = "UserComment"
)

var (
	errEXIFHeader = errors.New("exif: not a TIFF header")
	errEXIFEntry  = errors.New("exif: entry value lies outside the block")
	errEXIFType   = errors.New("exif: unknown entry type")
	errEXIFNoDirs = errors.New("exif: no image file directory")
)

// Real files carry one or two directories; the cap only bounds a hostile chain.
const maxEXIFDirs = 64

// val holds exactly size(typ)*count bytes, which the accessors rely on
// for bounds.
type exifTag struct {
	id    uint16
	typ   uint16
	count uint32
	val   []byte
	order binary.ByteOrder
}

type exifData struct {
	tags map[string]*exifTag
}

func (x *exifData) get(name string) (*exifTag, bool) {
	t, ok := x.tags[name]
	return t, ok
}

// Map order: a caller that renders the tags sorts them.
func (x *exifData) walk(fn func(name string, t *exifTag)) {
	for name, t := range x.tags {
		fn(name, t)
	}
}

// Every offset and length is checked against the block. Only the header
// and IFD0 must parse: past them, what fails to decode is dropped, which
// can lose tags but never surface a wrong one.
func decodeEXIF(data []byte) (*exifData, error) {
	order, first, err := exifHeader(data)
	if err != nil {
		return nil, err
	}
	dirs, err := exifDirs(data, order, first)
	if err != nil {
		return nil, err
	}

	x := &exifData{tags: make(map[string]*exifTag)}
	x.load(dirs[0], exifTagNames)
	// IFD1 is the embedded thumbnail's, so it has its own table.
	if len(dirs) > 1 {
		x.load(dirs[1], thumbTagNames)
	}

	subIFDs := []struct {
		pointer string
		names   map[uint16]string
	}{
		{exifIFDPointer, exifTagNames},
		{gpsIFDPointer, gpsTagNames},
		{interopIFDPointer, interopTagNames},
	}
	for _, sub := range subIFDs {
		x.loadSubIFD(data, order, sub.pointer, sub.names)
	}
	return x, nil
}

func exifHeader(data []byte) (binary.ByteOrder, uint64, error) {
	if len(data) < 8 {
		return nil, 0, errEXIFHeader
	}
	var order binary.ByteOrder
	switch string(data[0:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return nil, 0, errEXIFHeader
	}
	if order.Uint16(data[2:4]) != 42 {
		return nil, 0, errEXIFHeader
	}
	return order, uint64(order.Uint32(data[4:8])), nil
}

func exifDirs(data []byte, order binary.ByteOrder, off uint64) ([][]*exifTag, error) {
	var dirs [][]*exifTag
	seen := make(map[uint64]bool)
	for off != 0 && !seen[off] && len(dirs) < maxEXIFDirs {
		seen[off] = true
		tags, next, err := exifDir(data, order, off)
		if err != nil {
			break
		}
		dirs = append(dirs, tags)
		off = next
	}
	if len(dirs) == 0 {
		return nil, errEXIFNoDirs
	}
	return dirs, nil
}

func exifDir(data []byte, order binary.ByteOrder, off uint64) ([]*exifTag, uint64, error) {
	start := off
	if start+2 > uint64(len(data)) {
		return nil, 0, errEXIFEntry
	}
	n := uint64(order.Uint16(data[start : start+2]))
	// count word + n 12-byte entries + the next-directory offset.
	end := start + 2 + n*12 + 4
	if end > uint64(len(data)) {
		return nil, 0, errEXIFEntry
	}

	tags := make([]*exifTag, 0, n)
	for i := uint64(0); i < n; i++ {
		e := data[start+2+i*12:][:12]
		tag, err := exifEntry(data, order, e)
		if err != nil {
			return nil, 0, err
		}
		tags = append(tags, tag)
	}
	return tags, uint64(order.Uint32(data[end-4 : end])), nil
}

// A value of up to 4 bytes sits inline in the entry; a longer one at the
// offset it holds.
func exifEntry(data []byte, order binary.ByteOrder, e []byte) (*exifTag, error) {
	id := order.Uint16(e[0:2])
	typ := order.Uint16(e[2:4])
	count := order.Uint32(e[4:8])

	size, ok := exifTypeSize[typ]
	if !ok {
		return nil, errEXIFType
	}
	// In 64 bits, so a hostile count cannot wrap the length small.
	length := size * uint64(count)
	if length == 0 {
		return nil, errEXIFEntry
	}

	var val []byte
	if length > 4 {
		off := uint64(order.Uint32(e[8:12]))
		if off+length > uint64(len(data)) {
			return nil, errEXIFEntry
		}
		val = data[off : off+length]
	} else {
		val = e[8 : 8+length]
	}
	return &exifTag{id: id, typ: typ, count: count, val: val, order: order}, nil
}

// A later load overwrites an earlier one's tag, so a sub-IFD's copy wins
// over IFD0's.
func (x *exifData) load(tags []*exifTag, names map[uint16]string) {
	for _, t := range tags {
		if name, ok := names[t.id]; ok {
			x.tags[name] = t
		}
	}
}

func (x *exifData) loadSubIFD(data []byte, order binary.ByteOrder, pointer string, names map[uint16]string) {
	t, ok := x.get(pointer)
	if !ok {
		return
	}
	off, ok := t.intAt(0)
	if !ok || off < 0 || uint64(off) > uint64(len(data)) {
		return
	}
	tags, _, err := exifDir(data, order, uint64(off))
	if err != nil {
		return
	}
	x.load(tags, names)
}

func (t *exifTag) intAt(i int) (int64, bool) {
	vals := t.ints()
	if vals == nil || i >= len(vals) {
		return 0, false
	}
	return vals[i], true
}

func (t *exifTag) stringVal() (string, bool) {
	if t.typ != exifASCII {
		return "", false
	}
	if n := bytes.IndexByte(t.val, 0); n >= 0 {
		return string(t.val[:n]), true
	}
	return string(t.val), true
}

// UserComment opens with an 8-byte charset code. piexif, which A1111,
// Forge and the ComfyUI savers write through, stores "UNICODE\0" and
// UTF-16 as UNDEFINED; JIS is left undecoded.
func (t *exifTag) userCommentText() (string, bool) {
	if t.typ != exifASCII && t.typ != exifUndefined {
		return "", false
	}
	if len(t.val) >= 8 {
		switch body := t.val[8:]; string(t.val[:8]) {
		case "ASCII\x00\x00\x00", "\x00\x00\x00\x00\x00\x00\x00\x00":
			if n := bytes.IndexByte(body, 0); n >= 0 {
				body = body[:n]
			}
			return string(body), true
		case "UNICODE\x00":
			return decodeUTF16(body), true
		}
	}
	if t.typ == exifASCII {
		return t.stringVal()
	}
	return "", false
}

// A BOM decides the byte order; without one, mostly-ASCII text puts its
// zero bytes on the high side, and big-endian is piexif's own.
func decodeUTF16(b []byte) string {
	var order binary.ByteOrder = binary.BigEndian
	switch {
	case len(b) >= 2 && b[0] == 0xFE && b[1] == 0xFF:
		b = b[2:]
	case len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE:
		order, b = binary.LittleEndian, b[2:]
	default:
		var even, odd int
		for i, c := range b {
			if c == 0 {
				if i%2 == 0 {
					even++
				} else {
					odd++
				}
			}
		}
		if odd > even {
			order = binary.LittleEndian
		}
	}
	units := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		units = append(units, order.Uint16(b[i:]))
	}
	return strings.TrimRight(string(utf16.Decode(units)), "\x00")
}

func (t *exifTag) ints() []int64 {
	width, signed := 0, false
	switch t.typ {
	case exifByte:
		width = 1
	case exifSByte:
		width, signed = 1, true
	case exifShort:
		width = 2
	case exifSShort:
		width, signed = 2, true
	case exifLong:
		width = 4
	case exifSLong:
		width, signed = 4, true
	default:
		return nil
	}
	out := make([]int64, t.count)
	for i := range out {
		b := t.val[i*width:]
		var u uint64
		switch width {
		case 1:
			u = uint64(b[0])
		case 2:
			u = uint64(t.order.Uint16(b))
		case 4:
			u = uint64(t.order.Uint32(b))
		}
		if signed {
			shift := 64 - width*8
			out[i] = int64(u<<shift) >> shift
			continue
		}
		out[i] = int64(u)
	}
	return out
}

func (t *exifTag) floats() []float64 {
	out := make([]float64, t.count)
	for i := range out {
		switch t.typ {
		case exifFloat:
			out[i] = float64(math.Float32frombits(t.order.Uint32(t.val[i*4:])))
		case exifDouble:
			out[i] = math.Float64frombits(t.order.Uint64(t.val[i*8:]))
		default:
			return nil
		}
	}
	return out
}

func (t *exifTag) rats() [][2]int64 {
	out := make([][2]int64, t.count)
	for i := range out {
		b := t.val[i*8:]
		n, d := t.order.Uint32(b), t.order.Uint32(b[4:])
		switch t.typ {
		case exifRational:
			out[i] = [2]int64{int64(n), int64(d)}
		case exifSRational:
			out[i] = [2]int64{int64(int32(n)), int64(int32(d))}
		default:
			return nil
		}
	}
	return out
}

func (t *exifTag) String() string {
	body := t.render()
	if t.count == 1 {
		return strings.Trim(body, "[]")
	}
	return body
}

func (t *exifTag) render() string {
	if t.typ == exifASCII || t.typ == exifUndefined {
		return quotePrintable(t.val)
	}
	parts := make([]string, 0, t.count)
	switch t.typ {
	case exifRational, exifSRational:
		for _, r := range t.rats() {
			parts = append(parts, fmt.Sprintf(`"%v/%v"`, r[0], r[1]))
		}
	case exifFloat, exifDouble:
		for _, f := range t.floats() {
			parts = append(parts, fmt.Sprintf("%v", f))
		}
	default:
		for _, n := range t.ints() {
			parts = append(parts, fmt.Sprintf("%v", n))
		}
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// Bytes are dropped one at a time, which can split a rune, so an invalid
// UTF-8 result renders as empty.
func quotePrintable(in []byte) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, c := range in {
		if unicode.IsPrint(rune(c)) {
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	if s := b.String(); utf8.ValidString(s) {
		return s
	}
	return `""`
}

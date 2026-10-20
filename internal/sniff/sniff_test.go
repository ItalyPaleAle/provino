package sniff

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Build the "ftyp" box of an ISO media file with the given brands, the first one being the major brand
func ftyp(brands ...string) []byte {
	size := 16 + 4*(len(brands)-1)
	box := make([]byte, 0, size)
	box = binary.BigEndian.AppendUint32(box, uint32(size)) //nolint:gosec // The test sizes are tiny
	box = append(box, "ftyp"...)
	box = append(box, brands[0]...)
	box = append(box, 0, 0, 0, 0)
	for _, brand := range brands[1:] {
		box = append(box, brand...)
	}
	return box
}

func TestDetect(t *testing.T) {
	tests := map[string]struct {
		header      []byte
		format      string
		contentType string
	}{
		"jpeg":                       {header: []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F'}, format: "jpg", contentType: "image/jpeg"},
		"png":                        {header: []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n', 0, 0, 0, 13}, format: "png", contentType: "image/png"},
		"webp":                       {header: []byte("RIFF\x24\x00\x00\x00WEBPVP8 "), format: "webp", contentType: "image/webp"},
		"avif major brand":           {header: ftyp("avif", "mif1", "miaf"), format: "avif", contentType: "image/avif"},
		"avif as a compatible brand": {header: ftyp("mif1", "miaf", "avif"), format: "avif", contentType: "image/avif"},
		"avif sequence":              {header: ftyp("avis", "msf1"), format: "avif", contentType: "image/avif"},
		"heic":                       {header: ftyp("heic", "mif1", "heic"), format: "heic", contentType: "image/heic"},
		"heic as a compatible brand": {header: ftyp("mif1", "heic"), format: "heic", contentType: "image/heic"},
		"generic heif":               {header: ftyp("mif1", "miaf"), format: "heif", contentType: "image/heif"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			format, contentType, ok := Detect(tt.header)
			assert.True(t, ok)
			assert.Equal(t, tt.format, format)
			assert.Equal(t, tt.contentType, contentType)
		})
	}

	rejected := map[string][]byte{
		"empty":             nil,
		"gif":               []byte("GIF89a\x01\x00\x01\x00"),
		"svg":               []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`),
		"pdf":               []byte("%PDF-1.7\n"),
		"riff but not webp": []byte("RIFF\x24\x00\x00\x00WAVEfmt "),
		"mp4":               ftyp("isom", "iso2", "mp41"),
		"truncated ftyp":    []byte("\x00\x00\x00\x18ftypav"),
		"short ftyp size":   append([]byte{0, 0, 0, 8}, []byte("ftypavif\x00\x00\x00\x00")...),
	}
	for name, header := range rejected {
		t.Run("rejects "+name, func(t *testing.T) {
			_, _, ok := Detect(header)
			assert.False(t, ok)
		})
	}
}

func TestNormalizeFormat(t *testing.T) {
	accepted := map[string]string{
		"jpg":    "jpg",
		"jpeg":   "jpg",
		"JPEG":   "jpg",
		".JPG":   "jpg",
		" png ":  "png",
		"WebP":   "webp",
		".avif":  "avif",
		"heic":   "heic",
		"HEIF":   "heif",
		" .jpeg": "jpg",
	}
	for in, want := range accepted {
		got, ok := NormalizeFormat(in)
		assert.True(t, ok, in)
		assert.Equal(t, want, got, in)
	}

	for _, in := range []string{"", "gif", "svg", "jp", "..jpg", "image/jpeg"} {
		_, ok := NormalizeFormat(in)
		assert.False(t, ok, in)
	}
}

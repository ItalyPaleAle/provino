// Package sniff recognizes the image formats provino accepts as sources from their leading bytes
//
// A file's name and the content type a client declares are both under the client's control, so an upload's format is decided from its content alone
package sniff

import (
	"bytes"
	"encoding/binary"
)

// Formats are the source formats provino accepts, by the extension the original is stored with
var Formats = []string{"jpg", "png", "webp", "avif", "heic", "heif"}

// HeaderSize is how many leading bytes Detect needs to see
// It's enough for every signature, including the brand list of an ISO media file
const HeaderSize = 512

// Detect returns the format of a file from its leading bytes, along with its MIME type
// It returns false when the bytes don't start a file in one of the accepted formats
func Detect(header []byte) (format string, contentType string, ok bool) {
	switch {
	case bytes.HasPrefix(header, []byte{0xFF, 0xD8, 0xFF}):
		return "jpg", "image/jpeg", true
	case bytes.HasPrefix(header, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'}):
		return "png", "image/png", true
	case len(header) >= 12 && bytes.Equal(header[0:4], []byte("RIFF")) && bytes.Equal(header[8:12], []byte("WEBP")):
		return "webp", "image/webp", true
	}

	return detectISOMedia(header)
}

// Recognize AVIF and HEIF from the brands in the "ftyp" box that opens an ISO base media file
//
// The major brand names the specific format most of the time, but the generic image brands ("mif1", "msf1") leave it to the compatible brands, so every brand is considered: an AVIF brand anywhere makes the file AVIF, an HEVC brand makes it HEIC, and a file with only the generic brands is HEIF
func detectISOMedia(header []byte) (string, string, bool) {
	if len(header) < 16 || !bytes.Equal(header[4:8], []byte("ftyp")) {
		return "", "", false
	}

	// The box declares its own size, which bounds the brand list, and a box claiming to be larger than what was read is cut to what's available
	size := int(binary.BigEndian.Uint32(header[0:4]))
	if size < 16 {
		return "", "", false
	}
	size = min(size, len(header))

	// The major brand is at 8, the minor version at 12, and the compatible brands follow in groups of four bytes
	brands := make([]string, 0, (size-12)/4)
	brands = append(brands, string(header[8:12]))
	for offset := 16; offset+4 <= size; offset += 4 {
		brands = append(brands, string(header[offset:offset+4]))
	}

	var avif, heic, heif bool
	for _, brand := range brands {
		switch brand {
		case "avif", "avis":
			avif = true
		case "heic", "heix", "hevc", "hevx", "heim", "heis":
			heic = true
		case "mif1", "msf1":
			heif = true
		}
	}

	switch {
	case avif:
		return "avif", "image/avif", true
	case heic:
		return "heic", "image/heic", true
	case heif:
		return "heif", "image/heif", true
	default:
		return "", "", false
	}
}

// Package testimages supplies small encoded fixtures for image-reader tests.
package testimages

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"

	"golang.org/x/image/bmp"
)

// Encoded returns a 12 by 8 image in the requested test format.
func Encoded(name string) ([]byte, error) {
	img := image.NewNRGBA(image.Rect(0, 0, 12, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 12; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 20), G: uint8(y * 30), B: 100, A: 255})
		}
	}
	var out bytes.Buffer
	var err error
	switch name {
	case "sample.png":
		err = png.Encode(&out, img)
	case "sample.jpg":
		err = jpeg.Encode(&out, img, nil)
	case "sample.gif":
		err = gif.Encode(&out, img, nil)
	case "sample.bmp":
		err = bmp.Encode(&out, img)
	case "sample.webp":
		// Go has no WebP encoder; this 12 by 8 fixture was generated with Pillow.
		return base64.StdEncoding.DecodeString("UklGRjoAAABXRUJQVlA4IC4AAAAQAgCdASoMAAgAAUAmJaACdLoB+AH4AAPIAP6uF/9WfTQi/tI//Xog8Qb87IAA")
	default:
		return nil, fmt.Errorf("unknown image fixture %q", name)
	}
	return out.Bytes(), err
}

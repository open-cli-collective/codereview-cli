package pireviewtool

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"image"
	// Register only the image header readers accepted by Pi.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"

	// Register BMP and WebP headers without decoding pixels.
	_ "golang.org/x/image/bmp"
	"golang.org/x/image/vp8"
	"golang.org/x/image/vp8l"
	// Register the WebP header reader for image.DecodeConfig.
	_ "golang.org/x/image/webp"
)

const (
	maxImageBytes  = 8 * 1024 * 1024
	maxImagePixels = 16 * 1024 * 1024
	maxPNGChunks   = 1024
)

type imageSource struct {
	Data         string `json:"data"`
	MIMEType     string `json:"mimeType"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	SourceSHA256 string `json:"source_sha256"`
}

// readImageSource bounds the source before Pi's reader decodes or resizes it.
// This internal transport is larger than the reviewer-facing response budget.
func readImageSource(ctx context.Context, file *os.File, request Request) (string, error) {
	if request.Offset != 0 || request.Limit != 0 {
		return "", fmt.Errorf("%w: image views cannot be ranged", ErrDenied)
	}
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > maxImageBytes {
		return "", fmt.Errorf("%w: image exceeds the %d-byte input limit", ErrDenied, maxImageBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxImageBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxImageBytes {
		return "", fmt.Errorf("%w: image input limit exceeded", ErrDenied)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("%w: unsupported or invalid image header: %w", ErrDenied, err)
	}
	mimeTypes := map[string]string{"png": "image/png", "jpeg": "image/jpeg", "gif": "image/gif", "webp": "image/webp", "bmp": "image/bmp"}
	mimeType, supported := mimeTypes[format]
	if !supported {
		return "", fmt.Errorf("%w: unsupported image format %q", ErrDenied, format)
	}
	if config.Width <= 0 || config.Height <= 0 || int64(config.Width) > maxImagePixels/int64(config.Height) {
		return "", fmt.Errorf("%w: image exceeds the %d-pixel decode limit", ErrDenied, maxImagePixels)
	}
	if format == "png" {
		if err := validatePNGChunks(data); err != nil {
			return "", err
		}
	}
	if format == "gif" {
		if err := validateGIFFrames(ctx, data, config.Width, config.Height); err != nil {
			return "", err
		}
	}
	if format == "webp" {
		if err := validateWebPFrames(ctx, data, config.Width, config.Height); err != nil {
			return "", err
		}
	}
	sum := sha256.Sum256(data)
	output, err := json.Marshal(imageSource{Data: base64.StdEncoding.EncodeToString(data), MIMEType: mimeType, Width: config.Width, Height: config.Height, SourceSHA256: hex.EncodeToString(sum[:])})
	return string(output), err
}

func validatePNGChunks(data []byte) error {
	if len(data) < 8 || !bytes.Equal(data[:8], []byte("\x89PNG\r\n\x1a\n")) {
		return fmt.Errorf("%w: PNG signature required", ErrDenied)
	}
	chunks := 0
	for pos := 8; pos < len(data); {
		if len(data)-pos < 12 || chunks >= maxPNGChunks {
			break
		}
		size := binary.BigEndian.Uint32(data[pos:])
		if size > maxImageBytes || int64(size)+12 > int64(len(data)-pos) {
			break
		}
		end := pos + 8 + int(size)
		kind := string(data[pos+4 : pos+8])
		if crc32.ChecksumIEEE(data[pos+4:end]) != binary.BigEndian.Uint32(data[end:]) {
			return fmt.Errorf("%w: PNG chunk CRC mismatch", ErrDenied)
		}
		if kind == "acTL" {
			return fmt.Errorf("%w: animated PNG inspection is unsupported by Pi", ErrDenied)
		}
		chunks++
		pos = end + 4
		if kind == "IEND" {
			if size != 0 || pos != len(data) {
				break
			}
			return nil
		}
	}
	return fmt.Errorf("%w: incomplete or oversized PNG chunk inventory", ErrDenied)
}

// GIF frame dimensions can disagree with the logical screen returned by
// DecodeConfig. Validate them before Pi allocates a frame buffer.
func validateGIFFrames(ctx context.Context, data []byte, width, height int) error {
	invalid := func() error { return fmt.Errorf("%w: invalid or oversized GIF frame", ErrDenied) }
	if len(data) < 13 {
		return invalid()
	}
	colorTableSize := func(packed byte) int {
		if packed&0x80 == 0 {
			return 0
		}
		return 3 * (1 << (uint(packed&7) + 1))
	}
	pos := 13 + colorTableSize(data[10])
	skipBlocks := func() bool {
		for pos < len(data) {
			n := int(data[pos])
			pos++
			if n == 0 {
				return true
			}
			if n > len(data)-pos {
				return false
			}
			pos += n
		}
		return false
	}
	frames := 0
	for pos < len(data) {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch data[pos] {
		case 0x3b:
			if frames > 0 && pos+1 == len(data) {
				return nil
			}
			return invalid()
		case 0x21:
			if len(data)-pos < 2 {
				return invalid()
			}
			pos += 2
			if !skipBlocks() {
				return invalid()
			}
		case 0x2c:
			if len(data)-pos < 10 {
				return invalid()
			}
			left := int(binary.LittleEndian.Uint16(data[pos+1:]))
			top := int(binary.LittleEndian.Uint16(data[pos+3:]))
			w := int(binary.LittleEndian.Uint16(data[pos+5:]))
			h := int(binary.LittleEndian.Uint16(data[pos+7:]))
			if w <= 0 || h <= 0 || left+w > width || top+h > height {
				return invalid()
			}
			pos += 10 + colorTableSize(data[pos+9])
			if pos >= len(data) {
				return invalid()
			}
			pos++ // LZW minimum code size; Pi validates the compressed pixels.
			if !skipBlocks() {
				return invalid()
			}
			frames++
		default:
			return invalid()
		}
	}
	return invalid()
}

// VP8X canvas dimensions alone do not bound the pixel buffers in VP8/VP8L
// chunks. Check those frame headers, including frames inside ANMF chunks.
func validateWebPFrames(ctx context.Context, data []byte, width, height int) error {
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" || uint64(binary.LittleEndian.Uint32(data[4:]))+8 != uint64(len(data)) {
		return fmt.Errorf("%w: invalid WebP container", ErrDenied)
	}
	return validateWebPChunks(ctx, data[12:], width, height, true)
}

func validateWebPChunks(ctx context.Context, data []byte, width, height int, allowAnimation bool) error {
	invalid := func() error { return fmt.Errorf("%w: invalid or oversized WebP frame", ErrDenied) }
	frames := 0
	for len(data) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(data) < 8 {
			return invalid()
		}
		size := binary.LittleEndian.Uint32(data[4:])
		if size > maxImageBytes {
			return invalid()
		}
		padded := int(size) + int(size%2)
		if padded > len(data)-8 {
			return invalid()
		}
		payload := data[8 : 8+int(size)]
		switch string(data[:4]) {
		case "VP8 ":
			decoder := vp8.NewDecoder()
			decoder.Init(bytes.NewReader(payload), len(payload))
			header, err := decoder.DecodeFrameHeader()
			if err != nil || header.Width != width || header.Height != height {
				return invalid()
			}
			frames++
		case "VP8L":
			config, err := vp8l.DecodeConfig(bytes.NewReader(payload))
			if err != nil || config.Width != width || config.Height != height {
				return invalid()
			}
			frames++
		case "ANMF":
			if !allowAnimation || len(payload) < 16 {
				return invalid()
			}
			read24 := func(b []byte) int { return int(b[0]) | int(b[1])<<8 | int(b[2])<<16 }
			left, top := 2*read24(payload), 2*read24(payload[3:])
			w, h := 1+read24(payload[6:]), 1+read24(payload[9:])
			if w <= 0 || h <= 0 || left+w > width || top+h > height {
				return invalid()
			}
			if err := validateWebPChunks(ctx, payload[16:], w, h, false); err != nil {
				return err
			}
			frames++
		case "VP8X":
			if !allowAnimation || len(payload) != 10 {
				return invalid()
			}
			w := 1 + (int(payload[4]) | int(payload[5])<<8 | int(payload[6])<<16)
			h := 1 + (int(payload[7]) | int(payload[8])<<8 | int(payload[9])<<16)
			if w != width || h != height {
				return invalid()
			}
		}
		data = data[8+padded:]
	}
	if frames == 0 {
		return invalid()
	}
	return nil
}

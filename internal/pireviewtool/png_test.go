package pireviewtool

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestPNGViewReturnsNativeImageAndSourceEvidence(t *testing.T) {
	repo, diff := reviewerToolFixture(t)
	img := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	img.SetNRGBA(0, 0, color.NRGBA{R: 100, G: 50, B: 20, A: 0})
	img.SetNRGBA(1, 0, color.NRGBA{R: 255, A: 128})
	data := encodeTestPNG(t, img)
	writeTestPNG(t, repo, "image.png", data)
	config := Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: 4096}
	output, err := Execute(context.Background(), config, Request{Tool: ToolRead, Path: "image.png", View: "png"})
	if err != nil {
		t.Fatal(err)
	}
	result, meta := decodeTestPNGResult(t, output)
	if meta.SourceSHA256 != hashPNGBytes(data) || meta.Width != 2 || meta.Height != 1 || meta.PreviewResized || meta.RGBA16SHA256 == "" || len(meta.Chunks) == 0 {
		t.Fatalf("metadata = %+v", meta)
	}
	preview, err := base64.StdEncoding.DecodeString(result.Content[1].Data)
	if err != nil || !bytes.Equal(preview, data) {
		t.Fatalf("native preview differs from original: %v", err)
	}
	if _, err := Execute(context.Background(), config, Request{Tool: ToolRead, Path: "image.png"}); !errors.Is(err, ErrDenied) {
		t.Fatalf("ordinary binary read = %v", err)
	}
	img.SetNRGBA(0, 0, color.NRGBA{R: 101, G: 50, B: 20, A: 0})
	writeTestPNG(t, repo, "image.png", encodeTestPNG(t, img))
	output, err = Execute(context.Background(), config, Request{Tool: ToolRead, Path: "image.png", View: "png"})
	if err != nil {
		t.Fatal(err)
	}
	_, changed := decodeTestPNGResult(t, output)
	if meta.RGBA16SHA256 == changed.RGBA16SHA256 {
		t.Fatal("pixel hash ignored hidden RGB under zero alpha")
	}
}

func TestPNGViewSupportsPaletteAnd16BitImages(t *testing.T) {
	repo, diff := reviewerToolFixture(t)
	palette := image.NewPaletted(image.Rect(0, 0, 2, 1), color.Palette{color.Transparent, color.NRGBA{G: 255, A: 128}})
	palette.SetColorIndex(1, 0, 1)
	wide := image.NewNRGBA64(image.Rect(0, 0, 2, 1))
	wide.SetNRGBA64(0, 0, color.NRGBA64{R: 12345, G: 500, A: 65535})
	for index, img := range []image.Image{palette, wide} {
		writeTestPNG(t, repo, "image.png", encodeTestPNG(t, img))
		out, err := Execute(context.Background(), Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: 4096}, Request{Tool: ToolRead, Path: "image.png", View: "png"})
		if err != nil {
			t.Fatal(err)
		}
		_, meta := decodeTestPNGResult(t, out)
		if index == 1 {
			want := []byte{0x30, 0x39, 0x01, 0xf4, 0, 0, 0xff, 0xff, 0, 0, 0, 0, 0, 0, 0, 0}
			if meta.RGBA16SHA256 != hashPNGBytes(want) {
				t.Fatalf("16-bit pixel hash lost precision or byte order: %s", meta.RGBA16SHA256)
			}
		}
	}
}

func TestPNGViewLimitsPreviewDimensionsEvenWhenSourceFitsBudget(t *testing.T) {
	repo, diff := reviewerToolFixture(t)
	writeTestPNG(t, repo, "wide.png", encodeTestPNG(t, image.NewNRGBA(image.Rect(0, 0, 4096, 1))))
	out, err := Execute(context.Background(), Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: 8192}, Request{Tool: ToolRead, Path: "wide.png", View: "png"})
	if err != nil {
		t.Fatal(err)
	}
	_, metadata := decodeTestPNGResult(t, out)
	if metadata.PreviewWidth != maxPreviewEdge || !metadata.PreviewResized {
		t.Fatalf("unbounded preview dimensions: %+v", metadata)
	}
}

func TestPNGViewRejectsAnimationAndExcessiveChunks(t *testing.T) {
	valid := encodeTestPNG(t, image.NewNRGBA(image.Rect(0, 0, 1, 1)))
	chunk := func(kind string) []byte {
		data := make([]byte, 12)
		copy(data[4:], kind)
		binary.BigEndian.PutUint32(data[8:], crc32.ChecksumIEEE(data[4:8]))
		return data
	}
	for _, extra := range [][]byte{chunk("acTL"), bytes.Repeat(chunk("tEXt"), maxPNGChunks)} {
		data := append([]byte(nil), valid[:33]...)
		data = append(data, extra...)
		data = append(data, valid[33:]...)
		if _, err := inspectPNGChunks(data); !errors.Is(err, ErrDenied) {
			t.Fatalf("unsupported PNG accepted: %v", err)
		}
	}
}

func TestPNGViewBoundsPreviewWithoutTruncatingJSON(t *testing.T) {
	repo, diff := reviewerToolFixture(t)
	img := image.NewNRGBA(image.Rect(0, 0, 100, 100))
	seed := uint32(1234567)
	for i := range img.Pix {
		seed ^= seed << 13
		seed ^= seed >> 17
		seed ^= seed << 5
		img.Pix[i] = byte(seed & 255)
	}
	writeTestPNG(t, repo, "image.png", encodeTestPNG(t, img))
	const budget = 2048
	out, err := Execute(context.Background(), Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: budget}, Request{Tool: ToolRead, Path: "image.png", View: "png"})
	if err != nil {
		t.Fatal(err)
	}
	result, meta := decodeTestPNGResult(t, out)
	if len(out) > budget || !meta.PreviewResized || meta.Width != 100 || meta.PreviewWidth >= 100 {
		t.Fatalf("unbounded or unlabeled preview: %d %+v", len(out), meta)
	}
	data, err := base64.StdEncoding.DecodeString(result.Content[1].Data)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := png.Decode(bytes.NewReader(data))
	if err != nil || preview.Bounds().Dx() != meta.PreviewWidth {
		t.Fatalf("preview = %v, err = %v", preview, err)
	}
	_, err = Execute(context.Background(), Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: 100}, Request{Tool: ToolRead, Path: "image.png", View: "png"})
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("tiny output budget = %v", err)
	}
}

func TestPNGViewRejectsUnsafeAndInvalidInputs(t *testing.T) {
	repo, diff := reviewerToolFixture(t)
	valid := encodeTestPNG(t, image.NewNRGBA(image.Rect(0, 0, 1, 1)))
	badCRC := append([]byte(nil), valid...)
	badCRC[29] ^= 1
	bomb := append([]byte(nil), valid...)
	binary.BigEndian.PutUint32(bomb[16:], 32768)
	binary.BigEndian.PutUint32(bomb[20:], 32768)
	binary.BigEndian.PutUint32(bomb[29:], crc32.ChecksumIEEE(bomb[12:29]))
	for name, data := range map[string][]byte{"not-png": []byte("text"), "bad-crc": badCRC, "truncated": valid[:len(valid)-1], "trailing": append(append([]byte(nil), valid...), 1), "bomb": bomb, "oversize": make([]byte, maxPNGBytes+1)} {
		t.Run(name, func(t *testing.T) {
			writeTestPNG(t, repo, name+".png", data)
			_, err := Execute(context.Background(), Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: 4096}, Request{Tool: ToolRead, Path: name + ".png", View: "png"})
			if !errors.Is(err, ErrDenied) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	writeTestPNG(t, repo, "valid.png", valid)
	for _, req := range []Request{
		{Tool: ToolRead, Path: "../valid.png", View: "png"},
		{Tool: ToolRead, Path: filepath.Join(repo, "valid.png"), View: "png"},
		{Tool: ToolRead, Path: ".git/config", View: "png"},
		{Tool: ToolRead, Path: "valid.png", View: "png", Offset: 1},
		{Tool: ToolRead, Path: "valid.png", View: "png", Limit: 1},
	} {
		if _, err := Execute(context.Background(), Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: 4096}, req); !errors.Is(err, ErrDenied) {
			t.Fatalf("request %+v = %v", req, err)
		}
	}
	if err := os.Symlink(filepath.Join(repo, "valid.png"), filepath.Join(repo, "link.png")); err == nil {
		if _, err := Execute(context.Background(), Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: 4096}, Request{Tool: ToolRead, Path: "link.png", View: "png"}); !errors.Is(err, ErrDenied) {
			t.Fatalf("symlink read = %v", err)
		}
	}
}

func encodeTestPNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func writeTestPNG(t *testing.T, repo, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func decodeTestPNGResult(t *testing.T, out string) (pngResult, pngInspection) {
	t.Helper()
	var result pngResult
	var metadata pngInspection
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 2 || result.Content[0].Type != "text" || result.Content[1].Type != "image" || result.Content[1].MIMEType != "image/png" {
		t.Fatalf("invalid native result: %+v", result)
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &metadata); err != nil {
		t.Fatal(err)
	}
	return result, metadata
}

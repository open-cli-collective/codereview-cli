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
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/testimages"
)

func TestImageSourcePreservesBytesAndDimensions(t *testing.T) {
	repo, diff := reviewerToolFixture(t)
	for _, name := range []string{"sample.png", "sample.jpg", "sample.gif", "sample.webp", "sample.bmp"} {
		t.Run(name, func(t *testing.T) {
			data, err := testimages.Encoded(name)
			if err != nil {
				t.Fatal(err)
			}
			writeTestPNG(t, repo, "image", data)
			out, err := Execute(context.Background(), Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: 4096}, Request{Tool: ToolRead, Path: "image", View: "image"})
			if err != nil {
				t.Fatal(err)
			}
			var source imageSource
			if err := json.Unmarshal([]byte(out), &source); err != nil {
				t.Fatal(err)
			}
			decoded, err := base64.StdEncoding.DecodeString(source.Data)
			if err != nil || !bytes.Equal(decoded, data) || source.Width != 12 || source.Height != 8 || len(source.SourceSHA256) != 64 {
				t.Fatalf("invalid source: %+v, %v", source, err)
			}
			if _, err := Execute(context.Background(), Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: 4096}, Request{Tool: ToolRead, Path: "image"}); !errors.Is(err, ErrDenied) {
				t.Fatalf("ordinary binary read = %v", err)
			}
		})
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
		if err := validatePNGChunks(data); !errors.Is(err, ErrDenied) {
			t.Fatalf("unsupported PNG accepted: %v", err)
		}
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
	for name, data := range map[string][]byte{"not-png": []byte("text"), "bad-crc": badCRC, "truncated": valid[:len(valid)-1], "trailing": append(append([]byte(nil), valid...), 1), "bomb": bomb, "oversize": make([]byte, maxImageBytes+1)} {
		t.Run(name, func(t *testing.T) {
			writeTestPNG(t, repo, name+".png", data)
			_, err := Execute(context.Background(), Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: 4096}, Request{Tool: ToolRead, Path: name + ".png", View: "image"})
			if !errors.Is(err, ErrDenied) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	writeTestPNG(t, repo, "valid.png", valid)
	for _, req := range []Request{
		{Tool: ToolRead, Path: "../valid.png", View: "image"},
		{Tool: ToolRead, Path: filepath.Join(repo, "valid.png"), View: "image"},
		{Tool: ToolRead, Path: ".git/config", View: "image"},
		{Tool: ToolRead, Path: "valid.png", View: "image", Offset: 1},
		{Tool: ToolRead, Path: "valid.png", View: "image", Limit: 1},
	} {
		if _, err := Execute(context.Background(), Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: 4096}, req); !errors.Is(err, ErrDenied) {
			t.Fatalf("request %+v = %v", req, err)
		}
	}
	if err := os.Symlink(filepath.Join(repo, "valid.png"), filepath.Join(repo, "link.png")); err == nil {
		if _, err := Execute(context.Background(), Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: 4096}, Request{Tool: ToolRead, Path: "link.png", View: "image"}); !errors.Is(err, ErrDenied) {
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
	if err := os.WriteFile(filepath.Join(repo, name), data, 0o600); err != nil { // #nosec G703 -- test-owned filename under t.TempDir.
		t.Fatal(err)
	}
}

func TestImageSourceRejectsGIFFrameOutsideCanvas(t *testing.T) {
	repo, diff := reviewerToolFixture(t)
	data, err := testimages.Encoded("sample.gif")
	if err != nil {
		t.Fatal(err)
	}
	// Keep the existing frame but lie about the canvas dimensions.
	binary.LittleEndian.PutUint16(data[6:], 1)
	binary.LittleEndian.PutUint16(data[8:], 1)
	writeTestPNG(t, repo, "frame.gif", data)
	_, err = Execute(context.Background(), Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: 4096}, Request{Tool: ToolRead, Path: "frame.gif", View: "image"})
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("frame outside canvas accepted: %v", err)
	}
}

func TestImageSourceKeepsSVGAsText(t *testing.T) {
	repo, diff := reviewerToolFixture(t)
	writeTestPNG(t, repo, "image.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`))
	config := Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: 4096}
	if _, err := Execute(context.Background(), config, Request{Tool: ToolRead, Path: "image.svg", View: "image"}); !errors.Is(err, ErrDenied) {
		t.Fatalf("SVG image view = %v", err)
	}
	if out, err := Execute(context.Background(), config, Request{Tool: ToolRead, Path: "image.svg"}); err != nil || !bytes.Contains([]byte(out), []byte("<svg")) {
		t.Fatalf("SVG text read = %q, %v", out, err)
	}
}

func TestImageSourceRejectsWebPFrameOutsideCanvas(t *testing.T) {
	repo, diff := reviewerToolFixture(t)
	data, err := testimages.Encoded("sample.webp")
	if err != nil {
		t.Fatal(err)
	}
	extended := make([]byte, 18)
	copy(extended, "VP8X")
	binary.LittleEndian.PutUint32(extended[4:], 10)
	data = append(append(append([]byte{}, data[:12]...), extended...), data[12:]...)
	binary.LittleEndian.PutUint32(data[4:], uint32(len(data)-8)) // #nosec G115 -- tiny test fixture.
	writeTestPNG(t, repo, "frame.webp", data)
	_, err = Execute(context.Background(), Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: 4096}, Request{Tool: ToolRead, Path: "frame.webp", View: "image"})
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("frame outside canvas accepted: %v", err)
	}
	// A matching VP8X canvas still accepts the normal frame.
	data[24], data[27] = 11, 7
	writeTestPNG(t, repo, "frame.webp", data)
	if _, err := Execute(context.Background(), Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: 4096}, Request{Tool: ToolRead, Path: "frame.webp", View: "image"}); err != nil {
		t.Fatalf("valid extended WebP rejected: %v", err)
	}
}

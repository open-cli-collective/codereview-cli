package pireviewtool

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A shared-library fixture with a dependency, a loader path, an exported
// function, an executable segment and a memory-only section. No native tools
// or host-specific executable fixtures are required.
func nativeFixture(class64 bool, order binary.ByteOrder) []byte {
	data := make([]byte, 1024+7*64)
	copy(data, "\x7fELF")
	data[4], data[5], data[6] = 1, 1, 1
	if class64 {
		data[4] = 2
	}
	if order == binary.BigEndian {
		data[5] = 2
	}
	order.PutUint16(data[16:], uint16(elf.ET_DYN))
	order.PutUint16(data[18:], uint16(elf.EM_X86_64))
	order.PutUint32(data[20:], 1)
	shsize, symsize, dynsize := 40, 16, 8
	if class64 {
		shsize, symsize, dynsize = 64, 24, 16
		order.PutUint64(data[32:], 64)
		order.PutUint64(data[40:], 1024)
		for pos, value := range map[int]uint16{52: 64, 54: 56, 56: 2, 58: 64, 60: 7, 62: 1} {
			order.PutUint16(data[pos:], value)
		}
		order.PutUint32(data[64:], uint32(elf.PT_LOAD))
		order.PutUint32(data[68:], uint32(elf.PF_R|elf.PF_X))
		order.PutUint64(data[72:], 0)
		order.PutUint64(data[80:], 0x1000)
		order.PutUint64(data[96:], uint64(len(data)))
		order.PutUint64(data[104:], uint64(len(data))+4096)
	} else {
		order.PutUint32(data[28:], 64)
		order.PutUint32(data[32:], 1024)
		for pos, value := range map[int]uint16{40: 52, 42: 32, 44: 2, 46: 40, 48: 7, 50: 1} {
			order.PutUint16(data[pos:], value)
		}
		order.PutUint32(data[64:], uint32(elf.PT_LOAD))
		order.PutUint32(data[68:], 0)
		order.PutUint32(data[72:], 0x1000)
		order.PutUint32(data[80:], uint32(len(data)))      // #nosec G115 -- fixed small ELF fixture.
		order.PutUint32(data[84:], uint32(len(data))+4096) // #nosec G115 -- fixed small ELF fixture.
		order.PutUint32(data[88:], uint32(elf.PF_R|elf.PF_X))
	}
	names := "\x00.shstrtab\x00.dynstr\x00.dynsym\x00.dynamic\x00.text\x00.bss\x00"
	copy(data[256:], names)
	strtab := "\x00libc.so.6\x00napi_register_module_v1\x00$ORIGIN\x00"
	copy(data[384:], strtab)
	sections := []struct {
		name      string
		kind      elf.SectionType
		off, size uint64
		flags     elf.SectionFlag
		link      uint32
	}{
		{"", elf.SHT_NULL, 0, 0, 0, 0}, {".shstrtab", elf.SHT_STRTAB, 256, uint64(len(names)), 0, 0},
		{".dynstr", elf.SHT_STRTAB, 384, uint64(len(strtab)), 0, 0}, {".dynsym", elf.SHT_DYNSYM, 512, uint64(2 * symsize), 0, 2},
		{".dynamic", elf.SHT_DYNAMIC, 768, uint64(7 * dynsize), 0, 2}, {".text", elf.SHT_PROGBITS, 704, 16, elf.SHF_ALLOC | elf.SHF_EXECINSTR, 0},
		{".bss", elf.SHT_NOBITS, 720, 4096, elf.SHF_ALLOC | elf.SHF_WRITE, 0},
	}
	for i, s := range sections {
		raw := data[1024+i*shsize:]
		name := 0
		if s.name != "" {
			name = strings.Index(names, s.name)
		}
		order.PutUint32(raw, uint32(name))
		order.PutUint32(raw[4:], uint32(s.kind))
		if class64 {
			order.PutUint64(raw[8:], uint64(s.flags))
			order.PutUint64(raw[16:], 0x1000+s.off)
			order.PutUint64(raw[24:], s.off)
			order.PutUint64(raw[32:], s.size)
			order.PutUint32(raw[40:], s.link)
		} else {
			order.PutUint32(raw[8:], uint32(s.flags))
			order.PutUint32(raw[12:], 0x1000+uint32(s.off)) // #nosec G115 -- fixed small ELF fixture.
			order.PutUint32(raw[16:], uint32(s.off))        // #nosec G115 -- fixed synthetic fixture values fit in the ELF field.
			order.PutUint32(raw[20:], uint32(s.size))       // #nosec G115 -- fixed synthetic fixture values fit in the ELF field.
			order.PutUint32(raw[24:], s.link)
		}
	}
	sym := data[512+symsize:]
	order.PutUint32(sym, uint32(strings.Index(strtab, "napi_"))) // #nosec G115 -- fixed synthetic fixture values fit in the ELF field.
	if class64 {
		sym[4] = byte(elf.STB_GLOBAL)<<4 | byte(elf.STT_FUNC)
		order.PutUint16(sym[6:], 5)
	} else {
		sym[12] = byte(elf.STB_GLOBAL)<<4 | byte(elf.STT_FUNC)
		order.PutUint16(sym[14:], 5)
	}
	for i, entry := range [][2]uint64{{uint64(elf.DT_NEEDED), 1}, {uint64(elf.DT_RUNPATH), uint64(strings.Index(strtab, "$ORIGIN"))}, {uint64(elf.DT_STRTAB), 0x1000 + 384}, {uint64(elf.DT_STRSZ), uint64(len(strtab))}, {uint64(elf.DT_SYMTAB), 0x1000 + 512}, {uint64(elf.DT_SYMENT), uint64(symsize)}, {0, 0}} { // #nosec G115 -- fixed synthetic fixture values fit in the ELF field.
		raw := data[768+i*dynsize:]
		if class64 {
			order.PutUint64(raw, entry[0])
			order.PutUint64(raw[8:], entry[1])
		} else {
			order.PutUint32(raw, uint32(entry[0]))     // #nosec G115 -- fixed synthetic fixture values fit in the ELF field.
			order.PutUint32(raw[4:], uint32(entry[1])) // #nosec G115 -- fixed synthetic string offsets fit the ELF32 field.
		}
	}
	if class64 {
		order.PutUint32(data[120:], uint32(elf.PT_DYNAMIC))
		order.PutUint32(data[124:], uint32(elf.PF_R))
		order.PutUint64(data[128:], 768)
		order.PutUint64(data[136:], 0x1000+768)
		order.PutUint64(data[152:], uint64(7*dynsize))
		order.PutUint64(data[160:], uint64(7*dynsize))
	} else {
		order.PutUint32(data[96:], uint32(elf.PT_DYNAMIC))
		order.PutUint32(data[100:], 768)
		order.PutUint32(data[104:], 0x1000+768)
		order.PutUint32(data[112:], uint32(7*dynsize))
		order.PutUint32(data[116:], uint32(7*dynsize))
		order.PutUint32(data[120:], uint32(elf.PF_R)) // #nosec G115 -- fixed small ELF fixture.
	}
	copy(data[704:], []byte{0xc3, 0, 1, 2, 3})
	return data
}

func gzipNative(t *testing.T, data []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	writer := gzip.NewWriter(&out)
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestNativeArtifactReportsActualContent(t *testing.T) {
	for _, class64 := range []bool{false, true} {
		for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
			for _, compressed := range []bool{false, true} {
				t.Run(fmt.Sprintf("64=%t/order=%s/gzip=%t", class64, order, compressed), func(t *testing.T) {
					repo, diff := reviewerToolFixture(t)
					data := nativeFixture(class64, order)
					source := data
					if compressed {
						source = gzipNative(t, data)
					}
					writeTestPNG(t, repo, "library", source)
					out, err := Execute(context.Background(), Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: 16384}, Request{Tool: ToolRead, Path: "library", View: "binary"})
					if err != nil {
						t.Fatal(err)
					}
					var report nativeReport
					if err := json.Unmarshal([]byte(out), &report); err != nil {
						t.Fatal(err)
					}
					sum := sha256.Sum256(source)
					decoded := sha256.Sum256(data)
					if report.SourceSHA256 != hex.EncodeToString(sum[:]) || report.DecodedSHA256 != hex.EncodeToString(decoded[:]) || report.DecodedBytes != len(data) || report.Type != "ET_DYN" || len(report.Sections) != 7 || len(report.Segments) != 2 || report.Segments[0].Flags != "PF_X+PF_R" {
						t.Fatalf("bad report: %+v", report)
					}
					if len(report.Dependencies) != 1 || report.Dependencies[0] != "libc.so.6" || len(report.DynamicSymbols) != 1 || report.DynamicSymbols[0].Name != "napi_register_module_v1" || len(report.DynamicStrings) != 2 || report.DynamicStrings[1].Value != "$ORIGIN" {
						t.Fatalf("interface missing: %+v", report)
					}
					if report.Sections[6].SHA256 != "" || report.Sections[5].SHA256 != nativeHash(data[704:720]) || !strings.Contains(report.Note, "do not prove") {
						t.Fatalf("misleading report: %+v", report)
					}
				})
			}
		}
	}
}

func TestNativeArtifactPaginationAndHex(t *testing.T) {
	repo, diff := reviewerToolFixture(t)
	data := nativeFixture(true, binary.LittleEndian)
	writeTestPNG(t, repo, "lib.gz", gzipNative(t, data))
	config := Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: 700}
	var all strings.Builder
	for offset := int64(0); ; {
		out, err := Execute(context.Background(), config, Request{Tool: ToolRead, Path: "lib.gz", View: "binary", Offset: offset})
		if err != nil {
			t.Fatal(err)
		}
		if len(out) > config.MaxOutputBytes {
			t.Fatal("output cap exceeded")
		}
		header, body, ok := strings.Cut(out, "\n")
		if !ok {
			t.Fatal(out)
		}
		var start, end, total, next int64
		if _, err := fmt.Sscanf(header, "[cr-range offset=%d end=%d total=%d next_offset=%d]", &start, &end, &total, &next); err != nil {
			t.Fatal(err)
		}
		if start != offset || end-start != int64(len(body)) || end <= start {
			t.Fatal("invalid continuation")
		}
		all.WriteString(body)
		if next == -1 {
			break
		}
		offset = next
	}
	var report nativeReport
	if err := json.Unmarshal([]byte(all.String()), &report); err != nil {
		t.Fatal(err)
	}
	for offset := int64(0); ; {
		out, err := Execute(context.Background(), config, Request{Tool: ToolRead, Path: "lib.gz", View: "binary-hex", Offset: offset, Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		var chunk struct {
			Offset int64  `json:"offset"`
			End    int64  `json:"end"`
			Total  int64  `json:"total"`
			Next   int64  `json:"next_offset"`
			Hex    string `json:"hex"`
			Hash   string `json:"decoded_sha256"`
		}
		if err := json.Unmarshal([]byte(out), &chunk); err != nil {
			t.Fatal(err)
		}
		decoded, err := hex.DecodeString(chunk.Hex)
		if err != nil || len(out) > 700 || chunk.Offset != offset || chunk.End <= offset || chunk.Total != int64(len(data)) || chunk.Hash != report.DecodedSHA256 || !bytes.Equal(decoded, data[offset:chunk.End]) {
			t.Fatalf("invalid hex: %s", out)
		}
		if chunk.Next == -1 {
			break
		}
		offset = chunk.Next
	}
}

func TestNativeArtifactRejectsUnsafeInputs(t *testing.T) {
	repo, diff := reviewerToolFixture(t)
	valid := nativeFixture(true, binary.LittleEndian)
	corrupt := gzipNative(t, valid)
	corrupt[len(corrupt)-8] ^= 1
	bomb := gzipNative(t, make([]byte, maxNativeDecoded+1))
	for name, data := range map[string][]byte{"random": {0, 1, 2, 3}, "gzip-text": gzipNative(t, []byte("hello")), "truncated": valid[:100], "crc": corrupt, "trailing": append(gzipNative(t, valid), 1), "multimember": append(gzipNative(t, valid), gzipNative(t, valid)...), "bomb": bomb} {
		writeTestPNG(t, repo, name, data)
		for _, view := range []string{"binary", "binary-hex"} {
			if _, err := Execute(context.Background(), Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: 2048}, Request{Tool: ToolRead, Path: name, View: view}); !errors.Is(err, ErrDenied) {
				t.Fatalf("%s/%s: %v", name, view, err)
			}
		}
	}
	mutations := map[string]func([]byte){
		"extended-count":     func(b []byte) { binary.LittleEndian.PutUint16(b[60:], 0) },
		"section-offset":     func(b []byte) { binary.LittleEndian.PutUint64(b[40:], ^uint64(0)) },
		"section-size":       func(b []byte) { binary.LittleEndian.PutUint64(b[1024+5*64+32:], ^uint64(0)) },
		"compressed-section": func(b []byte) { binary.LittleEndian.PutUint64(b[1024+2*64+8:], uint64(elf.SHF_COMPRESSED)) },
		"bad-symbol-name":    func(b []byte) { binary.LittleEndian.PutUint32(b[536:], 100000) },
		"bad-symbol-link":    func(b []byte) { binary.LittleEndian.PutUint32(b[1024+3*64+40:], 6) },
		"bad-program":        func(b []byte) { binary.LittleEndian.PutUint64(b[96:], ^uint64(0)) },
		"bad-library":        func(b []byte) { binary.LittleEndian.PutUint64(b[776:], ^uint64(0)) },
	}
	for name, mutate := range mutations {
		b := bytes.Clone(valid)
		mutate(b)
		writeTestPNG(t, repo, name, b)
		if _, err := Execute(context.Background(), Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: 2048}, Request{Tool: ToolRead, Path: name, View: "binary"}); !errors.Is(err, ErrDenied) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	writeTestPNG(t, repo, "valid", valid)
	config := Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: 2048}
	for _, view := range []string{"binary", "binary-hex"} {
		for _, path := range []string{"../valid", filepath.Join(repo, "valid"), ".git/config"} {
			if _, err := Execute(context.Background(), config, Request{Tool: ToolRead, Path: path, View: view}); !errors.Is(err, ErrDenied) {
				t.Fatalf("%s: %v", path, err)
			}
		}
		for _, req := range []Request{{Tool: ToolRead, Path: "valid", View: view, Offset: -1}, {Tool: ToolRead, Path: "valid", View: view, Limit: -1}, {Tool: ToolRead, Path: "valid", View: view, Offset: 1 << 40}} {
			if _, err := Execute(context.Background(), config, req); !errors.Is(err, ErrDenied) {
				t.Fatalf("range accepted: %+v: %v", req, err)
			}
		}
		if err := os.Symlink(filepath.Join(repo, "valid"), filepath.Join(repo, "link-"+view)); err == nil {
			if _, err := Execute(context.Background(), config, Request{Tool: ToolRead, Path: "link-" + view, View: view}); !errors.Is(err, ErrDenied) {
				t.Fatal("symlink accepted")
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Execute(ctx, config, Request{Tool: ToolRead, Path: "valid", View: "binary"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := Execute(context.Background(), config, Request{Tool: ToolRead, Path: "valid"}); !errors.Is(err, ErrDenied) {
		t.Fatal("ordinary binary read was enabled")
	}
	file, err := os.Create(filepath.Join(repo, "oversize")) // #nosec G304 -- fixed fixture filename under t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxNativeInput + 1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Execute(context.Background(), config, Request{Tool: ToolRead, Path: "oversize", View: "binary"}); !errors.Is(err, ErrDenied) {
		t.Fatal("input limit bypassed")
	}
}

func FuzzNativeArtifactTables(f *testing.F) {
	f.Add(nativeFixture(true, binary.LittleEndian))
	f.Add(nativeFixture(false, binary.BigEndian))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > maxNativeInput {
			return
		}
		repo, diff := reviewerToolFixture(t)
		writeTestPNG(t, repo, "artifact", data)
		_, _ = Execute(context.Background(), Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: 2048}, Request{Tool: ToolRead, Path: "artifact", View: "binary"})
	})
}

func TestNativeArtifactRejectsMisleadingRuntimeTables(t *testing.T) {
	repo, diff := reviewerToolFixture(t)
	mutations := map[string]func([]byte){
		"different-dynamic-bytes":       func(b []byte) { binary.LittleEndian.PutUint64(b[128:], 704) },
		"missing-dynamic-program":       func(b []byte) { binary.LittleEndian.PutUint16(b[56:], 1) },
		"missing-dynamic-section":       func(b []byte) { binary.LittleEndian.PutUint32(b[1024+4*64+4:], uint32(elf.SHT_PROGBITS)) },
		"different-runtime-strings":     func(b []byte) { binary.LittleEndian.PutUint64(b[768+2*16+8:], 0x1000+704) },
		"different-runtime-string-size": func(b []byte) { binary.LittleEndian.PutUint64(b[768+3*16+8:], 1) },
		"different-runtime-symbols":     func(b []byte) { binary.LittleEndian.PutUint64(b[768+4*16+8:], 0x1000+704) },
		"different-symbol-stride":       func(b []byte) { binary.LittleEndian.PutUint64(b[768+5*16+8:], 16) },
		"unterminated-dynamic-table":    func(b []byte) { binary.LittleEndian.PutUint64(b[768+6*16:], uint64(elf.DT_DEBUG)) },
		"partial-string-mapping-overlap": func(b []byte) {
			binary.LittleEndian.PutUint16(b[56:], 3)
			binary.LittleEndian.PutUint32(b[176:], uint32(elf.PT_LOAD))
			binary.LittleEndian.PutUint64(b[184:], 900)
			binary.LittleEndian.PutUint64(b[192:], 0x1000+400)
			binary.LittleEndian.PutUint64(b[208:], 8)
			binary.LittleEndian.PutUint64(b[216:], 8)
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			data := nativeFixture(true, binary.LittleEndian)
			mutate(data)
			writeTestPNG(t, repo, name, data)
			for _, view := range []string{"binary", "binary-hex"} {
				if _, err := Execute(context.Background(), Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: 16384}, Request{Tool: ToolRead, Path: name, View: view}); !errors.Is(err, ErrDenied) {
					t.Fatalf("misleading %s accepted: %v", view, err)
				}
			}
		})
	}
}

func TestNativeArtifactIgnoresEntriesAfterDynamicTerminator(t *testing.T) {
	repo, diff := reviewerToolFixture(t)
	data := nativeFixture(true, binary.LittleEndian)
	binary.LittleEndian.PutUint64(data[1024+4*64+32:], 8*16)
	binary.LittleEndian.PutUint64(data[152:], 8*16)
	binary.LittleEndian.PutUint64(data[160:], 8*16)
	binary.LittleEndian.PutUint64(data[768+7*16:], uint64(elf.DT_NEEDED))
	binary.LittleEndian.PutUint64(data[768+7*16+8:], ^uint64(0))
	writeTestPNG(t, repo, "padding", data)
	out, err := Execute(context.Background(), Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: 16384}, Request{Tool: ToolRead, Path: "padding", View: "binary"})
	if err != nil {
		t.Fatal(err)
	}
	var report nativeReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Dependencies) != 1 || report.Dependencies[0] != "libc.so.6" || len(report.DynamicStrings) != 2 {
		t.Fatalf("post-terminator strings reported: %+v", report)
	}
}

func TestNativeArtifactRejectsEmptySymbolsWithInvalidStringTable(t *testing.T) {
	repo, diff := reviewerToolFixture(t)
	data := nativeFixture(true, binary.LittleEndian)
	// Remove the dynamic section/program so only symbol-table validation guards
	// the linked string range, even though there are no symbol entries to visit.
	binary.LittleEndian.PutUint16(data[56:], 1)
	binary.LittleEndian.PutUint32(data[1024+4*64+4:], uint32(elf.SHT_PROGBITS))
	binary.LittleEndian.PutUint64(data[1024+3*64+32:], 0)
	binary.LittleEndian.PutUint32(data[1024+3*64+40:], 6)
	binary.LittleEndian.PutUint64(data[1024+6*64+24:], uint64(len(data))+100)
	writeTestPNG(t, repo, "empty-symbols", data)
	for _, view := range []string{"binary", "binary-hex"} {
		if _, err := Execute(context.Background(), Config{RepoDir: repo, DiffPath: diff, MaxOutputBytes: 16384}, Request{Tool: ToolRead, Path: "empty-symbols", View: view}); !errors.Is(err, ErrDenied) {
			t.Fatalf("empty symbol string table accepted: %v", err)
		}
	}
}

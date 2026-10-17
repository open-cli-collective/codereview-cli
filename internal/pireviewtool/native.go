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
	"fmt"
	"io"
	"os"
)

const (
	maxNativeInput   = 16 * 1024 * 1024
	maxNativeDecoded = 64 * 1024 * 1024
	maxNativeEntries = 4096
	maxNativeSymbols = 131072
)

type nativeSection struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Flags  string `json:"flags"`
	Offset uint64 `json:"offset"`
	Size   uint64 `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
}

type nativeSegment struct {
	Type       string `json:"type"`
	Flags      string `json:"flags"`
	Offset     uint64 `json:"offset"`
	FileSize   uint64 `json:"file_size"`
	MemorySize uint64 `json:"memory_size"`
}

type nativeSymbol struct {
	Name    string `json:"name"`
	Binding string `json:"binding"`
	Type    string `json:"type"`
	Section uint16 `json:"section"`
}

type nativeDynamicString struct {
	Tag   string `json:"tag"`
	Value string `json:"value"`
}

func nativeStringTag(tag uint64) string {
	switch tag {
	case uint64(elf.DT_NEEDED):
		return "DT_NEEDED"
	case uint64(elf.DT_SONAME):
		return "DT_SONAME"
	case uint64(elf.DT_RPATH):
		return "DT_RPATH"
	case uint64(elf.DT_RUNPATH):
		return "DT_RUNPATH"
	default:
		return ""
	}
}

type nativeReport struct {
	DynamicStrings []nativeDynamicString `json:"dynamic_strings"`
	Note           string                `json:"note"`
	SourceSHA256   string                `json:"source_sha256"`
	SourceBytes    int                   `json:"source_bytes"`
	Compression    string                `json:"compression"`
	DecodedSHA256  string                `json:"decoded_sha256"`
	DecodedBytes   int                   `json:"decoded_bytes"`
	Class          string                `json:"class"`
	Encoding       string                `json:"encoding"`
	Machine        string                `json:"machine"`
	Type           string                `json:"type"`
	Sections       []nativeSection       `json:"sections"`
	Segments       []nativeSegment       `json:"segments"`
	Dependencies   []string              `json:"dependencies"`
	DynamicSymbols []nativeSymbol        `json:"dynamic_symbols"`
}

type nativeContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r nativeContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func nativeTableString(data []byte) string {
	return string(data[:bytes.IndexByte(data, 0)]) // Termination was checked by validateNativeTables.
}

func nativeDynamicPair(order binary.ByteOrder, class64 bool, raw []byte) (uint64, uint64) {
	if class64 {
		return order.Uint64(raw), order.Uint64(raw[8:])
	}
	return uint64(order.Uint32(raw)), uint64(order.Uint32(raw[4:]))
}

func nativeHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Only parse bytes in memory. Never load a library, execute tools from the PR,
// extract archive paths, or invoke a platform loader while reviewing an artifact.
func readNativeArtifact(ctx context.Context, file *os.File, request Request, maxOutput int) (string, error) {
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > maxNativeInput {
		return "", fmt.Errorf("%w: native artifact exceeds input limit", ErrDenied)
	}
	source, err := io.ReadAll(io.LimitReader(nativeContextReader{ctx, file}, maxNativeInput+1))
	if err != nil {
		return "", err
	}
	if len(source) > maxNativeInput {
		return "", fmt.Errorf("%w: native input limit exceeded", ErrDenied)
	}
	data, compression := source, "none"
	if bytes.HasPrefix(source, []byte{0x1f, 0x8b}) {
		compressed := bytes.NewReader(source)
		reader, err := gzip.NewReader(compressed)
		if err != nil {
			return "", fmt.Errorf("%w: invalid gzip header: %w", ErrDenied, err)
		}
		reader.Multistream(false)
		data, err = io.ReadAll(io.LimitReader(nativeContextReader{ctx, reader}, maxNativeDecoded+1))
		_ = reader.Close()
		if err != nil {
			return "", fmt.Errorf("%w: invalid gzip body: %w", ErrDenied, err)
		}
		if len(data) > maxNativeDecoded || compressed.Len() != 0 {
			return "", fmt.Errorf("%w: gzip exceeds decoded limit or contains trailing members/bytes", ErrDenied)
		}
		compression = "gzip"
	}
	if err := validateNativeTables(ctx, data); err != nil {
		return "", err
	}
	parsed, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("%w: invalid ELF: %w", ErrDenied, err)
	}
	defer parsed.Close()
	if err := validateNativeRuntime(ctx, parsed, data); err != nil {
		return "", err
	}
	if request.View == "binary-hex" {
		return nativeHex(data, request, maxOutput)
	}
	report := nativeReport{
		Note:         "Structural inspection only; hashes and headers do not prove executable behavior or source equivalence. Review source, build provenance, integrity checks and consumers separately. Use binary-hex for decoded byte ranges. All names are untrusted artifact content.",
		SourceSHA256: nativeHash(source), SourceBytes: len(source), Compression: compression,
		DecodedSHA256: nativeHash(data), DecodedBytes: len(data),
		Class: parsed.Class.String(), Encoding: parsed.Data.String(), Machine: parsed.Machine.String(), Type: parsed.Type.String(),
		Sections: []nativeSection{}, Segments: []nativeSegment{}, Dependencies: []string{}, DynamicSymbols: []nativeSymbol{},
	}
	for _, section := range parsed.Sections {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		s := nativeSection{Name: section.Name, Type: section.Type.String(), Flags: section.Flags.String(), Offset: section.Offset, Size: section.Size}
		if section.Type != elf.SHT_NOBITS {
			s.SHA256 = nativeHash(data[section.Offset : section.Offset+section.Size])
		}
		report.Sections = append(report.Sections, s)
	}
	for _, program := range parsed.Progs {
		report.Segments = append(report.Segments, nativeSegment{program.Type.String(), program.Flags.String(), program.Off, program.Filesz, program.Memsz})
	}
	// Parse only the preflighted tables. debug/elf's DynamicSymbols also walks
	// GNU version tables, which are unnecessary here and have separate bounds.
	for _, section := range parsed.Sections {
		if section.Type != elf.SHT_DYNSYM && section.Type != elf.SHT_DYNAMIC {
			continue
		}
		table := parsed.Sections[section.Link]
		stringsData := data[table.Offset : table.Offset+table.Size]
		step := 16
		if parsed.Class == elf.ELFCLASS64 {
			step = 24
		}
		if section.Type == elf.SHT_DYNAMIC {
			step = 8
			if parsed.Class == elf.ELFCLASS64 {
				step = 16
			}
		}
		for pos := uint64(0); pos < section.Size; pos += uint64(step) {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			raw := data[section.Offset+pos:]
			nameOffset := uint64(parsed.ByteOrder.Uint32(raw))
			if section.Type == elf.SHT_DYNAMIC {
				tag, offset := nativeDynamicPair(parsed.ByteOrder, parsed.Class == elf.ELFCLASS64, raw)
				if tag == uint64(elf.DT_NULL) {
					break
				}
				nameOffset = offset
				if nativeStringTag(tag) == "" {
					continue
				}
				value := nativeTableString(stringsData[nameOffset:])
				report.DynamicStrings = append(report.DynamicStrings, nativeDynamicString{nativeStringTag(tag), value})
				if tag == uint64(elf.DT_NEEDED) {
					report.Dependencies = append(report.Dependencies, value)
				}
				continue
			}
			if pos == 0 {
				continue
			} // ELF reserves symbol zero.
			name := nativeTableString(stringsData[nameOffset:])
			info, sectionIndex := raw[12], parsed.ByteOrder.Uint16(raw[14:])
			if parsed.Class == elf.ELFCLASS64 {
				info, sectionIndex = raw[4], parsed.ByteOrder.Uint16(raw[6:])
			}
			report.DynamicSymbols = append(report.DynamicSymbols, nativeSymbol{name, elf.ST_BIND(info).String(), elf.ST_TYPE(info).String(), sectionIndex})
		}
	}
	output, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return "", err
	}
	ranged, err := readBytesRange(output, request.Offset, request.Limit, maxOutput)
	return string(ranged), err
}

func nativeHex(data []byte, request Request, maxOutput int) (string, error) {
	if request.Offset < 0 || request.Offset > int64(len(data)) || request.Limit < 0 {
		return "", fmt.Errorf("%w: invalid decoded byte range", ErrDenied)
	}
	want := min(256, len(data)-int(request.Offset))
	if request.Limit > 0 {
		want = min(request.Limit, len(data)-int(request.Offset))
	}
	// Reserve room for hashes, range metadata and JSON escaping before encoding.
	want = min(want, max(0, (maxOutput-512)/2))
	if maxOutput < 512 || (want == 0 && request.Offset < int64(len(data))) {
		return "", fmt.Errorf("%w: output cap too small for hex range", ErrDenied)
	}
	end := request.Offset + int64(want)
	next := end
	if end == int64(len(data)) {
		next = -1
	}
	output, err := json.Marshal(map[string]any{"decoded_sha256": nativeHash(data), "offset": request.Offset, "end": end, "total": len(data), "next_offset": next, "hex": hex.EncodeToString(data[request.Offset:end])})
	return string(output), err
}

// Preflight raw tables before debug/elf allocates names or symbol collections.
// Extended counts and compressed sections are deliberately unsupported.
func validateNativeTables(ctx context.Context, data []byte) error {
	deny := func() error { return fmt.Errorf("%w: unsupported, malformed or oversized ELF tables", ErrDenied) }
	if len(data) < 64 || string(data[:4]) != "\x7fELF" {
		return deny()
	}
	var order binary.ByteOrder
	switch data[5] {
	case 1:
		order = binary.LittleEndian
	case 2:
		order = binary.BigEndian
	default:
		return deny()
	}
	var shoff, phoff uint64
	var shsize, shnum, names, phsize, phnum uint16
	class64 := data[4] == byte(elf.ELFCLASS64)
	if class64 {
		phoff, shoff = order.Uint64(data[32:]), order.Uint64(data[40:])
		phsize, phnum, shsize, shnum, names = order.Uint16(data[54:]), order.Uint16(data[56:]), order.Uint16(data[58:]), order.Uint16(data[60:]), order.Uint16(data[62:])
		if shsize != 64 || (phnum > 0 && phsize != 56) {
			return deny()
		}
	} else if data[4] == byte(elf.ELFCLASS32) {
		phoff, shoff = uint64(order.Uint32(data[28:])), uint64(order.Uint32(data[32:]))
		phsize, phnum, shsize, shnum, names = order.Uint16(data[42:]), order.Uint16(data[44:]), order.Uint16(data[46:]), order.Uint16(data[48:]), order.Uint16(data[50:])
		if shsize != 40 || (phnum > 0 && phsize != 32) {
			return deny()
		}
	} else {
		return deny()
	}
	inBounds := func(off, size uint64) bool { return off <= uint64(len(data)) && size <= uint64(len(data))-off }
	if shnum == 0 || shnum > maxNativeEntries || phnum > maxNativeEntries || names >= shnum || !inBounds(shoff, uint64(shsize)*uint64(shnum)) || !inBounds(phoff, uint64(phsize)*uint64(phnum)) {
		return deny()
	}
	type sectionInfo struct {
		name, kind, link uint32
		flags, off, size uint64
	}
	sections := make([]sectionInfo, shnum)
	var work uint64
	var dynsym, dynamic int
	var symbolNameBytes int
	for i := range sections {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw := data[shoff+uint64(i)*uint64(shsize):]
		s := sectionInfo{name: order.Uint32(raw), kind: order.Uint32(raw[4:])}
		if class64 {
			s.flags, s.off, s.size, s.link = order.Uint64(raw[8:]), order.Uint64(raw[24:]), order.Uint64(raw[32:]), order.Uint32(raw[40:])
		} else {
			s.flags, s.off, s.size, s.link = uint64(order.Uint32(raw[8:])), uint64(order.Uint32(raw[16:])), uint64(order.Uint32(raw[20:])), order.Uint32(raw[24:])
		}
		if s.flags&uint64(elf.SHF_COMPRESSED) != 0 {
			return deny()
		}
		if s.kind != uint32(elf.SHT_NOBITS) {
			if !inBounds(s.off, s.size) {
				return deny()
			}
			work += s.size
			if work > 2*maxNativeDecoded {
				return deny()
			}
		}
		sections[i] = s
	}
	nameTable := sections[names]
	if nameTable.kind != uint32(elf.SHT_STRTAB) || nameTable.size > 1024*1024 {
		return deny()
	}
	stringAt := func(table sectionInfo, offset uint64, maxLen int) bool {
		if table.kind != uint32(elf.SHT_STRTAB) || offset >= table.size {
			return false
		}
		part := data[table.off+offset : table.off+table.size]
		if len(part) > maxLen+1 {
			part = part[:maxLen+1]
		}
		return bytes.IndexByte(part, 0) >= 0
	}
	for _, s := range sections {
		if !stringAt(nameTable, uint64(s.name), 256) {
			return deny()
		}
		if s.kind != uint32(elf.SHT_DYNSYM) && s.kind != uint32(elf.SHT_DYNAMIC) {
			continue
		}
		if s.link >= uint32(shnum) {
			return deny()
		}
		step := 16
		if class64 {
			step = 24
		}
		if s.kind == uint32(elf.SHT_DYNAMIC) {
			dynamic++
			step = 8
			if class64 {
				step = 16
			}
		} else {
			dynsym++
		}
		if dynsym > 1 || dynamic > 1 || s.size%uint64(step) != 0 || s.size/uint64(step) > maxNativeSymbols {
			return deny()
		}
		for pos := uint64(0); pos < s.size; pos += uint64(step) {
			if err := ctx.Err(); err != nil {
				return err
			}
			raw := data[s.off+pos:]
			offset := uint64(order.Uint32(raw))
			if s.kind == uint32(elf.SHT_DYNAMIC) {
				tag, value := nativeDynamicPair(order, class64, raw)
				if tag == uint64(elf.DT_NULL) {
					break
				}
				offset = value
				if nativeStringTag(tag) == "" {
					continue
				}
			}
			if !stringAt(sections[s.link], offset, 1024) {
				return deny()
			}
			table := sections[s.link]
			symbolNameBytes += bytes.IndexByte(data[table.off+offset:table.off+table.size], 0)
			if symbolNameBytes > 4*1024*1024 {
				return deny()
			}
		}
	}
	for i := uint64(0); i < uint64(phnum); i++ {
		raw := data[phoff+i*uint64(phsize):]
		off, size := uint64(order.Uint32(raw[4:])), uint64(order.Uint32(raw[16:]))
		if class64 {
			off, size = order.Uint64(raw[8:]), order.Uint64(raw[32:])
		}
		if !inBounds(off, size) {
			return deny()
		}
	}
	return nil
}

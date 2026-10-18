package pireviewtool

import (
	"context"
	"debug/elf"
	"fmt"
)

// Section metadata is author-controlled. Cross-check the dynamic table and
// string/symbol locations against the program headers a runtime loader uses.
func validateNativeRuntime(ctx context.Context, parsed *elf.File, data []byte) error {
	deny := func() error {
		return fmt.Errorf("%w: ELF runtime and section tables disagree or are unsupported", ErrDenied)
	}
	section := parsed.SectionByType(elf.SHT_DYNAMIC)
	var program *elf.Prog
	for _, p := range parsed.Progs {
		if p.Type == elf.PT_DYNAMIC {
			if program != nil {
				return deny()
			}
			program = p
		}
	}
	if (section == nil) != (program == nil) {
		return deny()
	}
	if section == nil {
		return nil
	}
	// Reject ambiguous mappings rather than selecting a convenient PT_LOAD.
	mapsTo := func(address, size, offset uint64) bool {
		matches := 0
		for _, p := range parsed.Progs {
			if p.Type != elf.PT_LOAD {
				continue
			}
			if p.Filesz > p.Memsz {
				return false
			}
			if address < p.Vaddr {
				if size > p.Vaddr-address && p.Memsz > 0 {
					return false
				}
				continue
			}
			delta := address - p.Vaddr
			if delta >= p.Memsz {
				continue
			}
			if delta > p.Filesz || size > p.Filesz-delta || p.Off+delta != offset {
				return false
			}
			matches++
		}
		return matches == 1
	}
	if section.Offset != program.Off || section.Size != program.Filesz || section.Addr != program.Vaddr || program.Filesz > program.Memsz || !mapsTo(section.Addr, section.Size, section.Offset) {
		return deny()
	}
	values := make(map[elf.DynTag]uint64)
	step, symsize := uint64(8), uint64(16)
	if parsed.Class == elf.ELFCLASS64 {
		step, symsize = 16, 24
	}
	terminated := false
	for pos := uint64(0); pos < section.Size; pos += step {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw := data[section.Offset+pos:]
		tag, value := nativeDynamicPair(parsed.ByteOrder, parsed.Class == elf.ELFCLASS64, raw)
		if tag == uint64(elf.DT_NULL) {
			terminated = true
			break
		}
		for _, wanted := range []elf.DynTag{elf.DT_STRTAB, elf.DT_STRSZ, elf.DT_SYMTAB, elf.DT_SYMENT} {
			if tag != uint64(wanted) { // #nosec G115 -- wanted is one of four positive ELF tag constants.
				continue
			}
			if _, exists := values[wanted]; exists {
				return deny()
			}
			values[wanted] = value
		}
	}
	if !terminated {
		return deny()
	}
	strtab := parsed.Sections[section.Link]
	straddr, hasStraddr := values[elf.DT_STRTAB]
	strsize, hasStrsize := values[elf.DT_STRSZ]
	if !hasStraddr || !hasStrsize || straddr != strtab.Addr || strsize != strtab.Size || !mapsTo(straddr, strsize, strtab.Offset) {
		return deny()
	}
	symbols := parsed.SectionByType(elf.SHT_DYNSYM)
	symaddr, hasSymaddr := values[elf.DT_SYMTAB]
	syment, hasSyment := values[elf.DT_SYMENT]
	if symbols == nil {
		if hasSymaddr || hasSyment {
			return deny()
		}
		return nil
	}
	if symbols.Link != section.Link || !hasSymaddr || !hasSyment || syment != symsize || symaddr != symbols.Addr || !mapsTo(symaddr, symbols.Size, symbols.Offset) {
		return deny()
	}
	return nil
}

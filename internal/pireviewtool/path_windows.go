//go:build windows

package pireviewtool

import (
	"os"
	"syscall"
)

func isLinkLike(info os.FileInfo) bool {
	if info.Mode()&os.ModeSymlink != 0 {
		return true
	}
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && data.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

// A path below the root can change Windows volumes only through a reparse
// point, which is rejected by isLinkLike at every component.
func sameFileSystem(_, _ os.FileInfo) bool { return true }

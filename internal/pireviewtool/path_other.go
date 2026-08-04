//go:build !unix && !windows

package pireviewtool

import "os"

func isLinkLike(info os.FileInfo) bool { return info.Mode()&os.ModeSymlink != 0 }

func sameFileSystem(_, _ os.FileInfo) bool { return true }

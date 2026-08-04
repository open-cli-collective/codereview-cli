//go:build unix

package pireviewtool

import (
	"os"
	"syscall"
)

func isLinkLike(info os.FileInfo) bool {
	return info.Mode()&os.ModeSymlink != 0
}

func sameFileSystem(root, candidate os.FileInfo) bool {
	rootStat, rootOK := root.Sys().(*syscall.Stat_t)
	candidateStat, candidateOK := candidate.Sys().(*syscall.Stat_t)
	return rootOK && candidateOK && rootStat.Dev == candidateStat.Dev
}

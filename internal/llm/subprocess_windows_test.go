//go:build windows

package llm

import (
	"errors"

	"golang.org/x/sys/windows"
)

func processExists(pid int) bool {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return !errors.Is(err, windows.ERROR_INVALID_PARAMETER)
	}
	_ = windows.CloseHandle(process)
	return true
}

//go:build windows

package runlock

import "testing"

func TestWindowsLockRangeIsSingleByteAtZero(t *testing.T) {
	lockRange := windowsByteRange()
	if lockRange.offsetLow != 0 || lockRange.offsetHigh != 0 || lockRange.lengthLow != 1 || lockRange.lengthHigh != 0 {
		t.Fatalf("windows lock range = %#v, want byte 0 length 1", lockRange)
	}
}

//go:build windows

package runlock

import "testing"

func TestWindowsLockRangeIsSingleByteAtZero(t *testing.T) {
	if windowsLockOffsetLow != 0 || windowsLockOffsetHigh != 0 || windowsLockLengthLow != 1 || windowsLockLengthHigh != 0 {
		t.Fatalf("windows lock range = offset %d/%d length %d/%d, want byte 0 length 1", windowsLockOffsetLow, windowsLockOffsetHigh, windowsLockLengthLow, windowsLockLengthHigh)
	}
}

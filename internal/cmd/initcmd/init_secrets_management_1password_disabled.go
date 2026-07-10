//go:build keyring_no1password

package initcmd

// Some distributions intentionally compile out 1Password support, such as
// compliance-scoped or air-gapped builds. The init UX uses this build tag gate
// to hide unavailable backends rather than exposing runtime-only failures.
func initOnePasswordBackendsAvailable() bool {
	return false
}

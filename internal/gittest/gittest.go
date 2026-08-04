// Package gittest builds hermetic environments for git commands run by tests.
package gittest

import "os"

// Env returns the process environment with host git configuration masked, so
// fixture repositories behave identically regardless of the developer's
// global or system config. Without this, host settings leak into fixture
// commits — commit signing with a passphrase-protected key fails every
// fixture commit, and hooksPath or init.templateDir can rewrite fixture
// state — making the suite pass or fail by machine.
func Env() []string {
	return append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
	)
}

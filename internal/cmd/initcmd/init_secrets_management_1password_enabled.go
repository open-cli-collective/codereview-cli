//go:build !keyring_no1password

package initcmd

func initOnePasswordBackendsAvailable() bool {
	return true
}

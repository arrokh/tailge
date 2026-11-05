//go:build !darwin

package discovery

func darwinProcessStartIdentity(int) string { return "" }

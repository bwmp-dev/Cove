//go:build !linux

package worker

func syncDirectory(string) error { return nil }

//go:build !linux

package worker

func inspectMounts() ([]MountMetric, bool) { return nil, true }

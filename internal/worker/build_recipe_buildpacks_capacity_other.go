//go:build !linux

package worker

import "errors"

func inspectBuildpacksCapacity(state, dockerRoot string) (BuildpacksCapacity, error) {
	return BuildpacksCapacity{}, errors.New("Cloud Native Buildpacks requires a Linux build worker")
}

func verifyBuildpacksWorkFilesystem() error {
	return errors.New("Cloud Native Buildpacks requires Linux disk-backed operation state")
}

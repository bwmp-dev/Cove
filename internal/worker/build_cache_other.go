//go:build !linux

package worker

import (
	"errors"
	"io"
)

func BuildCacheFilesystem(io.Writer) error {
	return errors.New("hard build-cache verification requires Linux")
}

func inspectCacheFilesystem(string, string) (cacheFilesystem, uint64, error) {
	return cacheFilesystem{}, 0, errors.New("hard build-cache verification requires Linux")
}

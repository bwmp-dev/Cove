//go:build !linux

package worker

import (
	"errors"
	"os"
)

func startTerminal(string, uint16, uint16) (*os.File, func(uint16, uint16) error, func(), error) {
	return nil, nil, nil, errors.New("node terminals require Linux")
}

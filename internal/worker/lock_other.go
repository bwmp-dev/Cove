//go:build !linux

package worker

import "errors"

func lockWorker(string) (func(), error) { return nil, errors.New("the worker daemon requires Linux") }

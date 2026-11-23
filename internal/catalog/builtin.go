package catalog

import (
	"crypto/sha256"
	_ "embed"
	"fmt"
	"sync"
)

//go:embed builtin.json
var builtinData []byte

var builtinOnce sync.Once
var builtinIndex Index
var builtinError error

func Builtin() (Index, string, error) {
	builtinOnce.Do(func() { builtinIndex, builtinError = Decode(builtinData) })
	return builtinIndex, fmt.Sprintf("%x", sha256.Sum256(builtinData)), builtinError
}

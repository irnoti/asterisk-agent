//go:build !linux

package tui

import (
	"errors"
	"os"
)

func makeRaw(*os.File) (func(), error) { return nil, errors.New("raw terminal not supported here") }
func termWidth(*os.File) int           { return 80 }

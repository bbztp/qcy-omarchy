//go:build !linux

package main

import (
	"errors"
	"os"
)

func rawTerminal(fd int) (func(), error) {
	return nil, errors.New("hearing run needs Linux")
}

func shrinkPipe(f *os.File) {}

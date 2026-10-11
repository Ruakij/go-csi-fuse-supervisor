//go:build !linux

package fusemount

import (
	"errors"
	"os"
)

var errUnsupported = errors.New("fusemount needs Linux")

func statfsState(string) (State, error) { return Other, errUnsupported }

func unmount(string) error { return errUnsupported }

func bind(string, string, bool) error { return errUnsupported }

func replaceIn(*os.File, string, string, bool) error { return errUnsupported }

//go:build !windows

package remote

import (
	"errors"
	"io"
)

func dialPageant() (io.ReadWriteCloser, error) {
	return nil, errors.New("Pageant is only available on Windows")
}

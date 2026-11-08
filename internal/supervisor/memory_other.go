//go:build !linux && !darwin

package supervisor

import "errors"

func freeMemory() (free, total int64, err error) {
	return 0, 0, errors.New("free memory is not read on this system")
}

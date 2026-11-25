//go:build !linux

package timesync

import (
	"errors"
	"time"
)

// Apply — часы подстраиваются только в гостевой ОС (Linux).
func Apply(offset time.Duration, action Action) error {
	return errors.New("подстройка часов поддерживается только в Linux")
}

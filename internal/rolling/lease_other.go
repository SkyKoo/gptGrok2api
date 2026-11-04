//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package rolling

import (
	"errors"
	"os"
)

// Keep ordinary CFM builds portable. Rolling activation requires a host with
// the reviewed flock semantics and must fail closed elsewhere.
func acquireLease(string) (*os.File, error) {
	return nil, errors.New("rolling leases unsupported on this platform")
}
func releaseLease(*os.File) error { return errors.New("rolling leases unsupported on this platform") }

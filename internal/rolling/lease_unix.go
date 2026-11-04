//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package rolling

import (
	"os"
	"syscall"
)

func acquireLease(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}
func releaseLease(file *os.File) error {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
		return err
	}
	return file.Close()
}

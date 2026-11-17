//go:build darwin || linux

package main

import (
	"errors"
	"os"
	"syscall"
)

func flock(f *os.File, nonblocking bool) error {
	mode := syscall.LOCK_EX
	if nonblocking {
		mode |= syscall.LOCK_NB
	}
	for {
		err := syscall.Flock(int(f.Fd()), mode)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

func funlock(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }

func links(info os.FileInfo) uint64 { return uint64(info.Sys().(*syscall.Stat_t).Nlink) }

func diskSize(info os.FileInfo) int64 { return info.Sys().(*syscall.Stat_t).Blocks * 512 }

func busy(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}

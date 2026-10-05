//go:build linux

package namespace

import (
	"fmt"
	"runtime"
	"syscall"
)

const targetNSNet = 0x00000000

var setnsNumbers = map[string]uintptr{
	"amd64":   308,
	"386":     346,
	"arm":     375,
	"arm64":   268,
	"ppc64":   350,
	"ppc64le": 350,
	"s390x":   339,
	"riscv64": 268,
}

func setns(fd int, target int) error {
	number, ok := setnsNumbers[runtime.GOARCH]
	if !ok {
		return fmt.Errorf("setns is not known on %s", runtime.GOARCH)
	}
	if _, _, errno := syscall.Syscall(number, uintptr(fd), uintptr(target), 0); errno != 0 {
		return errno
	}
	return nil
}

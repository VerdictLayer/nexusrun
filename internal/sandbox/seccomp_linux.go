//go:build linux

package sandbox

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Landlock only governs TCP, and only on kernels with ABI 4 or newer. A unit
// that did not declare the network capability must not be able to reach the
// network at all, so a seccomp filter additionally refuses to create any
// socket that is not a local (AF_UNIX) or netlink socket. That closes UDP, DNS,
// raw and packet sockets on every kernel, regardless of the Landlock ABI.
//
// The filter is inherited across exec and cannot be removed.

const (
	auditArchX8664   = 0xc000003e
	auditArchAArch64 = 0xc00000b7

	seccompRetKillProcess = 0x80000000
	seccompRetErrno       = 0x00050000 // | errno
	seccompRetAllow       = 0x7fff0000

	afUnix    = 1
	afNetlink = 16

	// x32 syscalls share the x86-64 audit arch but set this bit in the number.
	x32SyscallBit = 0x40000000

	// Offsets into struct seccomp_data.
	offNr   = 0
	offArch = 4
	offArg0 = 16

	bpfLD  = 0x00
	bpfJMP = 0x05
	bpfRET = 0x06
	bpfW   = 0x00
	bpfABS = 0x20
	bpfJEQ = 0x10
	bpfJGE = 0x30
	bpfK   = 0x00
)

func auditArch() (uint32, error) {
	switch runtime.GOARCH {
	case "amd64":
		return auditArchX8664, nil
	case "arm64":
		return auditArchAArch64, nil
	}
	return 0, fmt.Errorf("no network filter for %s; refusing to run without one", runtime.GOARCH)
}

// networkFilter returns the classic-BPF program that denies socket(2) for every
// domain except AF_UNIX and AF_NETLINK.
func networkFilter(arch uint32) []unix.SockFilter {
	stmt := func(code uint16, k uint32) unix.SockFilter { return unix.SockFilter{Code: code, K: k} }
	jump := func(code uint16, k uint32, jt, jf uint8) unix.SockFilter {
		return unix.SockFilter{Code: code, Jt: jt, Jf: jf, K: k}
	}
	return []unix.SockFilter{
		/* 0 */ stmt(bpfLD|bpfW|bpfABS, offArch),
		/* 1 */ jump(bpfJMP|bpfJEQ|bpfK, arch, 0, 8), // wrong architecture: kill
		/* 2 */ stmt(bpfLD|bpfW|bpfABS, offNr),
		/* 3 */ jump(bpfJMP|bpfJGE|bpfK, x32SyscallBit, 6, 0), // x32 ABI: kill
		/* 4 */ jump(bpfJMP|bpfJEQ|bpfK, uint32(unix.SYS_SOCKET), 0, 4), // not socket(2): allow
		/* 5 */ stmt(bpfLD|bpfW|bpfABS, offArg0),
		/* 6 */ jump(bpfJMP|bpfJEQ|bpfK, afUnix, 2, 0),
		/* 7 */ jump(bpfJMP|bpfJEQ|bpfK, afNetlink, 1, 0),
		/* 8 */ stmt(bpfRET|bpfK, seccompRetErrno|uint32(unix.EACCES)),
		/* 9 */ stmt(bpfRET|bpfK, seccompRetAllow),
		/* 10 */ stmt(bpfRET|bpfK, seccompRetKillProcess),
	}
}

// denyNetwork installs the filter on every thread of this process.
func denyNetwork() error {
	arch, err := auditArch()
	if err != nil {
		return err
	}
	prog := networkFilter(arch)
	fprog := unix.SockFprog{Len: uint16(len(prog)), Filter: &prog[0]}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	// TSYNC applies the filter to all threads, which matters because the Go
	// runtime runs goroutines on several.
	if _, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&fprog))); errno != 0 {
		return fmt.Errorf("install seccomp filter: %w", errno)
	}
	return nil
}

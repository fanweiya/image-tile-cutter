package main

import (
	"syscall"
	"unsafe"
)

// Shell file operation constants (shellapi.h).
const (
	foDelete          = 0x0003
	fofSilent         = 0x0004
	fofNoConfirmation = 0x0010
	fofAllowUndo      = 0x0040
	fofNoErrorUI      = 0x0400
)

const maxLongPath = 240

type shFileOpStructW struct {
	hwnd                  uintptr
	wFunc                 uint32
	pFrom                 *uint16
	pTo                   *uint16
	fFlags                uint16
	fAnyOperationsAborted int32
	hNameMappings         uintptr
	lpszProgressTitle     *uint16
}

var (
	modshell32           = syscall.NewLazyDLL("shell32.dll")
	procSHFileOperationW = modshell32.NewProc("SHFileOperationW")
)

// longPath returns a path usable beyond MAX_PATH by the Win32 file APIs.
func longPath(path string) string {
	if len(path) < maxLongPath {
		return path
	}
	if len(path) >= 4 && path[0] == '\\' && path[1] == '\\' && (path[2] == '?' || path[2] == '.') {
		return path
	}
	abs, err := syscall.FullPath(path)
	if err != nil {
		return path
	}
	if len(abs) >= 2 && abs[1] == ':' {
		return `\\?\` + abs
	}
	return `\\?\UNC\` + abs[2:]
}

// moveToRecycleBin moves the file or directory at path to the Recycle Bin,
// so that a mistaken delete can still be undone from Explorer, instead of
// removing it from the disk for good.
func moveToRecycleBin(path string) error {
	abs, err := syscall.FullPath(path)
	if err != nil {
		return err
	}
	from, err := syscall.UTF16FromString(abs)
	if err != nil {
		return err
	}
	// SHFileOperationW takes a double-null terminated list of paths.
	from = append(from, 0)
	op := shFileOpStructW{
		wFunc:  foDelete,
		pFrom:  &from[0],
		fFlags: fofAllowUndo | fofNoConfirmation | fofSilent | fofNoErrorUI,
	}
	ret, _, _ := procSHFileOperationW.Call(uintptr(unsafe.Pointer(&op)))
	if ret != 0 {
		return syscall.Errno(ret)
	}
	if op.fAnyOperationsAborted != 0 {
		return syscall.EINVAL
	}
	return nil
}

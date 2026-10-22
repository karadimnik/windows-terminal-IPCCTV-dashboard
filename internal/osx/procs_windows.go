//go:build windows

package osx

import (
	"errors"
	"syscall"
	"unsafe"
)

// ListProcs takes a Toolhelp snapshot of the processes: image name and parent pid. Read only.
func ListProcs() ([]ProcEntry, error) {
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer syscall.CloseHandle(snap)
	var pe syscall.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	var out []ProcEntry
	for err = syscall.Process32First(snap, &pe); err == nil; err = syscall.Process32Next(snap, &pe) {
		out = append(out, ProcEntry{Pid: int(pe.ProcessID), PPid: int(pe.ParentProcessID), Name: syscall.UTF16ToString(pe.ExeFile[:])})
	}
	if !errors.Is(err, syscall.ERROR_NO_MORE_FILES) {
		return out, err
	}
	return out, nil
}

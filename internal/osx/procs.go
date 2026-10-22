package osx

// ProcEntry is one entry of the OS process list: enough to find a process that no registry lists (doctor).
type ProcEntry struct {
	Pid  int
	PPid int
	Name string // the executable's base name as the OS lists it (comm on Linux, the image name on Windows)
}

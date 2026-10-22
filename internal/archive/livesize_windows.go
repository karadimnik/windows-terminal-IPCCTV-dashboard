//go:build windows

package archive

import "syscall"

// LiveSize is Get-CamLiveFileLength (R:2555-2573): NTFS refreshes a file's directory entry only when
// the writer closes its handle, so a listing (os.ReadDir, FindFirstFile) shows a stale size for the
// segment ffmpeg is writing. Opening the file and asking the handle gives the real size. The open
// reads only and shares read, write and delete, so it never blocks ffmpeg or a delete.
func LiveSize(path string) (int64, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return -1, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return -1, err
	}
	defer syscall.CloseHandle(h)
	var fi syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &fi); err != nil {
		return -1, err
	}
	return int64(fi.FileSizeHigh)<<32 | int64(fi.FileSizeLow), nil
}

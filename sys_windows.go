//go:build windows

package main

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"
)

var (
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	shell32              = syscall.NewLazyDLL("shell32.dll")
	procGetLogicalDrives = kernel32.NewProc("GetLogicalDrives")
	procGetDriveType     = kernel32.NewProc("GetDriveTypeW")
	procDiskFree         = kernel32.NewProc("GetDiskFreeSpaceExW")
	procSHFileOperation  = shell32.NewProc("SHFileOperationW")
)

const (
	attrReparse      = 0x400
	attrOffline      = 0x1000
	attrRecallOnData = 0x400000
)

func attrs(info fs.FileInfo) uint32 {
	if d, ok := info.Sys().(*syscall.Win32FileAttributeData); ok {
		return d.FileAttributes
	}
	return 0
}

// Junctions and directory links are skipped so nothing is counted twice and there are no loops.
// OneDrive folders are reparse points too, but Readlink fails on them, so they are scanned.
func isLinkDir(path string, info fs.FileInfo) bool {
	if attrs(info)&attrReparse == 0 {
		return false
	}
	_, err := os.Readlink(path)
	return err == nil
}

// Cloud-only files (OneDrive, not downloaded) take no disk space.
func cloudOnly(info fs.FileInfo) bool {
	return attrs(info)&(attrOffline|attrRecallOnData) != 0
}

func samePath(a, b string) bool { return strings.EqualFold(a, b) }

func diskSpace(path string) (total, free uint64) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return
	}
	var avail uint64
	procDiskFree.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&avail)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&free)))
	return
}

type driveInfo struct {
	Path  string `json:"path"`
	Total uint64 `json:"total"`
	Free  uint64 `json:"free"`
}

func listDrives() []driveInfo {
	out := []driveInfo{}
	mask, _, _ := procGetLogicalDrives.Call()
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		p, _ := syscall.UTF16PtrFromString(root)
		t, _, _ := procGetDriveType.Call(uintptr(unsafe.Pointer(p)))
		if t != 2 && t != 3 && t != 4 { // removable, fixed, network
			continue
		}
		total, free := diskSpace(root)
		if total == 0 {
			continue
		}
		out = append(out, driveInfo{root, total, free})
	}
	return out
}

type shFileOp struct {
	hwnd      uintptr
	wFunc     uint32
	pFrom     *uint16
	pTo       *uint16
	fFlags    uint16
	fAborted  int32
	hNameMap  uintptr
	lpszTitle *uint16
}

func moveToTrash(path string) error {
	u, err := syscall.UTF16FromString(path)
	if err != nil {
		return err
	}
	u = append(u, 0) // double-null-terminated list
	op := shFileOp{
		wFunc: 3, // FO_DELETE
		pFrom: &u[0],
		// FOF_ALLOWUNDO | FOF_NOCONFIRMATION | FOF_NOERRORUI | FOF_WANTNUKEWARNING:
		// goes to the Recycle Bin; if it does not fit, Windows warns before deleting for good.
		fFlags: 0x40 | 0x10 | 0x400 | 0x4000,
	}
	r, _, _ := procSHFileOperation.Call(uintptr(unsafe.Pointer(&op)))
	if r != 0 {
		return fmt.Errorf("%d", r)
	}
	if op.fAborted != 0 {
		return fmt.Errorf("cancelled")
	}
	return nil
}

func revealInExplorer(path string) {
	cmd := exec.Command("explorer.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer.exe /select,"` + path + `"`}
	cmd.Start()
}

func openBrowser(url string) {
	exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

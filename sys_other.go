//go:build !windows

package main

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

func isLinkDir(path string, info fs.FileInfo) bool { return false }
func cloudOnly(info fs.FileInfo) bool              { return false }
func samePath(a, b string) bool                    { return a == b }

func diskSpace(path string) (total, free uint64) {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) == nil {
		total = st.Blocks * uint64(st.Bsize)
		free = st.Bavail * uint64(st.Bsize)
	}
	return
}

type driveInfo struct {
	Path  string `json:"path"`
	Total uint64 `json:"total"`
	Free  uint64 `json:"free"`
}

func listDrives() []driveInfo {
	t, f := diskSpace("/")
	return []driveInfo{{"/", t, f}}
}

func moveToTrash(path string) error {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".local", "share", "Trash", "files")
	os.MkdirAll(dir, 0o755)
	return os.Rename(path, filepath.Join(dir, filepath.Base(path)))
}

func revealInExplorer(path string) { exec.Command("xdg-open", filepath.Dir(path)).Start() }
func openBrowser(url string)       { exec.Command("xdg-open", url).Start() }

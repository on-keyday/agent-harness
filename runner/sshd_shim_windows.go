//go:build windows

package runner

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"unsafe"
)

// stageSSHDShim returns an absolute path to an executable named sshd.exe that
// behaves exactly as cmd.exe, staging it if it is not already present.
//
// It exists for one reason: ms-vscode-remote.remote-ssh's Windows bootstrap
// (out/resolver.js) opens with `$global:sshdPID = getSshdParentPid`, which walks
// its own process' parent chain and keeps the first ancestor whose image name is
// `sshd`. Reached through the harness ssh gateway there is no sshd anywhere in
// the chain — the leaf runs under agent-runner — so the walk finds nothing, the
// bootstrap prints "no sshd parent proc" and exits 0, and Remote-SSH silently
// gives up. Running the shell line under a process whose *file name* is sshd.exe
// (Windows derives the process name from the image file name) puts an ancestor
// named `sshd` in the chain, and the walk succeeds. Measured on this host on
// 2026-08-25 and again against this staged path: only the process NAME matters,
// so a byte copy of cmd.exe under the name sshd.exe is sufficient — no real
// sshd, no custom binary. cmd.exe does not care what it is called, so exit
// codes, stdout/stderr separation and `/c` quoting are identical (verified on A).
//
// The caller (shellLineArgv) substitutes the returned path for the literal
// "cmd" in `{"cmd", "/c", line}`, so the whole change is a different image path
// for the same argv shape.
//
// Safe to call concurrently and repeatedly: the common path is a stat compare
// with no write, and staging goes through a same-directory temp + atomic replace
// guarded by a process mutex, so racing execs cannot observe a half-written file.
func stageSSHDShim() (string, error) {
	src, err := systemCmdExe()
	if err != nil {
		return "", err
	}
	srcInfo, err := os.Stat(src)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", src, err)
	}

	dir, err := shimDir()
	if err != nil {
		return "", err
	}
	dest := filepath.Join(dir, "sshd.exe")

	// Fast path: an existing copy whose size matches the live cmd.exe is taken
	// as good and returned without touching the disk. The dest is only ever
	// created by the atomic replace below — a fully written temp renamed into
	// place — so any file present at this name is a complete copy, and a size
	// match against the current cmd.exe means it is a copy of THIS cmd.exe
	// rather than a stale one left by an OS update. A full content hash on every
	// exec would buy essentially nothing against that: the only way to get a
	// same-size-but-different file here is an external actor writing an exact
	// 344 KB look-alike, which is not a case the fast path owes a defense.
	if fi, err := os.Stat(dest); err == nil && fi.Size() == srcInfo.Size() {
		return dest, nil
	}

	shimStageMu.Lock()
	defer shimStageMu.Unlock()

	// Re-check under the lock: a concurrent caller in this process may have
	// staged it between our stat above and acquiring the mutex.
	if fi, err := os.Stat(dest); err == nil && fi.Size() == srcInfo.Size() {
		return dest, nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}

	tmp, err := copyToTemp(dir, src)
	if err != nil {
		return "", err
	}
	// From here tmp must not leak: either it becomes dest, or we remove it.
	if err := atomicReplace(tmp, dest); err != nil {
		// A cross-process race is the expected loser here: another runner (or a
		// prior crashed one's successor) may have won the replace, or hold the
		// old dest open because it is mid-exec. If dest now matches the live
		// cmd.exe, that peer produced a correct copy and we are done — drop our
		// temp and use it.
		if fi, serr := os.Stat(dest); serr == nil && fi.Size() == srcInfo.Size() {
			_ = os.Remove(tmp)
			return dest, nil
		}
		_ = os.Remove(tmp)
		return "", err
	}
	return dest, nil
}

// shimStageMu serializes staging within this process; atomicReplace handles the
// cross-process case.
var shimStageMu sync.Mutex

// shimDir is the per-runner directory the shim is staged into.
//
// %LOCALAPPDATA% (what os.UserCacheDir returns on Windows), not %TEMP% and not
// the runner's BinDir:
//   - user-writable: the runner is non-admin (measured ADMIN=False on A), so a
//     machine-wide location such as %ProgramFiles% or System32 is out.
//   - stable across runner restarts and reboots: %TEMP% is subject to Disk
//     Cleanup / Storage Sense and, in some service contexts, is per-logon and
//     wiped — a home that can vanish between runs. %LOCALAPPDATA% persists.
//   - not the BinDir: BinDir is prepended to the agent's PATH (agentenv.go), so
//     dropping an sshd.exe there would make a bare `sshd` an agent types resolve
//     to this cmd shim — a surprising side effect for an unrelated dir.
//   - not a task worktree: the shim is per-runner, and a worktree is per-task
//     and gets wiped on cleanup.
//
// A wipe of %TEMP% in the fallback path is self-healing: the next exec finds no
// dest and re-stages, so correctness never depends on the fallback surviving.
func shimDir() (string, error) {
	if cache, err := os.UserCacheDir(); err == nil && cache != "" {
		return filepath.Join(cache, "agent-harness", "shim"), nil
	}
	// Degraded fallback for the rare context where %LOCALAPPDATA% is unset (some
	// bare service accounts). Less stable, but self-healing as noted above, and
	// strictly better than failing every exec.
	return filepath.Join(os.TempDir(), "agent-harness", "shim"), nil
}

// systemCmdExe resolves System32\cmd.exe by asking the OS for the system
// directory, never a bare "cmd" through PATH: a name lookup could resolve to a
// different cmd.exe earlier on PATH (this project has been bitten by bare
// Windows binary-name resolution before), and %ComSpec% is user-overridable —
// GetSystemDirectory is neither.
func systemCmdExe() (string, error) {
	buf := make([]uint16, syscall.MAX_PATH)
	// GetSystemDirectoryW returns the length in UTF-16 units written (excluding
	// the NUL) on success, the required length if buf is too small, or 0 on
	// failure. MAX_PATH is always enough for the real system directory.
	r, _, err := procGetSystemDirectoryW.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	n := uint32(r)
	if n == 0 {
		return "", fmt.Errorf("GetSystemDirectoryW: %w", err)
	}
	if int(n) > len(buf) {
		buf = make([]uint16, n)
		r, _, err = procGetSystemDirectoryW.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		n = uint32(r)
		if n == 0 || int(n) > len(buf) {
			return "", fmt.Errorf("GetSystemDirectoryW (retry): %w", err)
		}
	}
	sysDir := syscall.UTF16ToString(buf[:n])
	return filepath.Join(sysDir, "cmd.exe"), nil
}

// copyToTemp writes a byte copy of src to a freshly created temp file in dir and
// returns its path. The temp shares dir with the final name so the subsequent
// replace is a same-volume rename (atomic) rather than a cross-volume move.
func copyToTemp(dir, src string) (path string, err error) {
	in, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()

	tmp, err := os.CreateTemp(dir, "sshd-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create temp in %s: %w", dir, err)
	}
	name := tmp.Name()
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		_ = os.Remove(name)
		return "", fmt.Errorf("copy cmd.exe: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("close temp: %w", err)
	}
	return name, nil
}

// atomicReplace renames tmp onto dest, replacing an existing dest atomically.
//
// os.Rename cannot do the replace on Windows — it fails when the target exists —
// so this goes to MoveFileExW with MOVEFILE_REPLACE_EXISTING. WRITE_THROUGH
// flushes the rename to disk before returning, so a crash right after cannot
// leave dest pointing at freed data.
func atomicReplace(tmp, dest string) error {
	const (
		moveReplaceExisting = 0x1
		moveWriteThrough    = 0x8
	)
	from, err := syscall.UTF16PtrFromString(tmp)
	if err != nil {
		return err
	}
	to, err := syscall.UTF16PtrFromString(dest)
	if err != nil {
		return err
	}
	r, _, e := procMoveFileExW.Call(
		uintptr(unsafe.Pointer(from)),
		uintptr(unsafe.Pointer(to)),
		uintptr(moveReplaceExisting|moveWriteThrough),
	)
	if r == 0 {
		return fmt.Errorf("MoveFileExW %s -> %s: %w", tmp, dest, e)
	}
	return nil
}

var (
	modkernel32             = syscall.NewLazyDLL("kernel32.dll")
	procGetSystemDirectoryW = modkernel32.NewProc("GetSystemDirectoryW")
	procMoveFileExW         = modkernel32.NewProc("MoveFileExW")
)

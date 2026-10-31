package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// CheckSHA256 reports whether data hashes to want (lowercase hex).
func CheckSHA256(data []byte, want string) error {
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("SHA-256 mismatch: got %s, want %s", got, want)
	}
	return nil
}

// ExtractBinary returns the machine-monitor binary from a release archive: a .tar.gz, or a
// .zip for Windows (machine-monitor.exe).
func ExtractBinary(archiveName string, data []byte) ([]byte, error) {
	if strings.HasSuffix(archiveName, ".zip") {
		return extractZip(data)
	}
	return extractTarGz(data)
}

func isBinary(name string) bool {
	b := path.Base(name)
	return b == "machine-monitor" || b == "machine-monitor.exe"
}

func extractTarGz(data []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("archive has no machine-monitor binary")
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && isBinary(h.Name) {
			return readLimited(tr)
		}
	}
}

func extractZip(data []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if !f.Mode().IsRegular() || !isBinary(f.Name) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return readLimited(rc)
	}
	return nil, errors.New("archive has no machine-monitor binary")
}

func readLimited(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxBinary+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxBinary {
		return nil, errors.New("binary in archive is too large")
	}
	return b, nil
}

// Replace swaps the binary at exe for bin. The new file is written next to exe and renamed
// over it, so exe is always either the old or the new binary, never half of one. Windows
// can't overwrite a running .exe but can rename it, so there the old binary is moved aside
// to exe+".old" first (CleanOld removes it on a later run).
func Replace(exe string, bin []byte) error {
	return replace(exe, bin, runtime.GOOS == "windows")
}

func replace(exe string, bin []byte, renameAside bool) (err error) {
	mode := fs.FileMode(0o755)
	if fi, err := os.Stat(exe); err == nil {
		mode = fi.Mode().Perm() | 0o100
	}
	f, err := os.CreateTemp(filepath.Dir(exe), ".machine-monitor-new-*")
	if err != nil {
		return fmt.Errorf("can't write next to %s: %w", exe, err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(bin); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	if !renameAside {
		return os.Rename(tmp, exe)
	}
	old := exe + ".old"
	if err := os.Remove(old); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", old, err)
	}
	if err := os.Rename(exe, old); err != nil {
		return err
	}
	if err := os.Rename(tmp, exe); err != nil {
		if rerr := os.Rename(old, exe); rerr != nil {
			return fmt.Errorf("%w (and putting the old binary back failed: %v)", err, rerr)
		}
		return err
	}
	return nil
}

// CleanOld removes exe+".old" left by an earlier Windows update. It fails silently: the old
// binary may still be running.
func CleanOld(exe string) { os.Remove(exe + ".old") }

// ManagedBy names the package manager that owns exe ("Homebrew", "Scoop"), or "" when the
// binary is the user's to replace.
func ManagedBy(exe string) string {
	p := filepath.ToSlash(exe)
	switch {
	case strings.Contains(p, "/Cellar/") || strings.Contains(p, "/Caskroom/"):
		return "Homebrew"
	case strings.Contains(strings.ToLower(p), "/scoop/apps/"):
		return "Scoop"
	}
	return ""
}

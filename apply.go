package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// chownLike gives path the same owner as ref. Only possible as root; failures are ignored.
func chownLike(path string, ref os.FileInfo) {
	if ref == nil || os.Geteuid() != 0 {
		return
	}
	if st, ok := ref.Sys().(*syscall.Stat_t); ok {
		_ = os.Lchown(path, int(st.Uid), int(st.Gid))
	}
}

// copyFileAtomic replaces dst with src. The new content is written next to dst and
// renamed into place, so readers never see a half-written file.
func copyFileAtomic(src, dst string, mode os.FileMode) error {
	old, _ := os.Stat(dst)
	if mode == 0 {
		mode = 0o644
		if old != nil {
			mode = old.Mode().Perm()
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".bp-tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	chownLike(tmp, old)
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// extractReplace unpacks the archive into a staging directory beside dst, then swaps it in.
func extractReplace(archive, assetName, dst string) error {
	dst = filepath.Clean(dst)
	staging, backup := dst+".bp-new", dst+".bp-old"
	os.RemoveAll(staging)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return err
	}

	var n int
	var err error
	switch {
	case strings.HasSuffix(assetName, ".tar.gz"), strings.HasSuffix(assetName, ".tgz"):
		n, err = extractTarGz(archive, staging)
	case strings.HasSuffix(assetName, ".zip"):
		n, err = extractZip(archive, staging)
	default:
		err = fmt.Errorf("extract_to supports .tar.gz, .tgz and .zip, got %q", assetName)
	}
	if err == nil && n == 0 {
		err = fmt.Errorf("archive %s is empty", assetName)
	}
	if err != nil {
		os.RemoveAll(staging)
		return err
	}

	old, _ := os.Stat(dst)
	if old != nil {
		_ = filepath.Walk(staging, func(p string, _ os.FileInfo, err error) error {
			if err == nil {
				chownLike(p, old)
			}
			return nil
		})
	}

	os.RemoveAll(backup)
	if old != nil {
		if err := os.Rename(dst, backup); err != nil {
			os.RemoveAll(staging)
			return fmt.Errorf("move old directory aside: %w", err)
		}
	}
	if err := os.Rename(staging, dst); err != nil {
		if old != nil {
			_ = os.Rename(backup, dst) // put the old one back
		}
		os.RemoveAll(staging)
		return err
	}
	os.RemoveAll(backup)
	return nil
}

// safeJoin rejects entries that would escape root ("zip slip").
func safeJoin(root, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe path in archive: %q", name)
	}
	return filepath.Join(root, clean), nil
}

func writeEntry(path string, mode os.FileMode, r io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm()|0o400)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func extractTarGz(archive, dst string) (int, error) {
	f, err := os.Open(archive)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return 0, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	count := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return count, nil
		}
		if err != nil {
			return count, err
		}
		if filepath.Clean(h.Name) == "." {
			continue
		}
		target, err := safeJoin(dst, h.Name)
		if err != nil {
			return count, err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = os.MkdirAll(target, 0o755)
		case tar.TypeReg:
			err = writeEntry(target, h.FileInfo().Mode(), tr)
			count++
		default:
			slog.Warn("skipping unsupported archive entry", "name", h.Name, "type", string(h.Typeflag))
		}
		if err != nil {
			return count, err
		}
	}
}

func extractZip(archive, dst string) (int, error) {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return 0, err
	}
	defer zr.Close()
	count := 0
	for _, zf := range zr.File {
		target, err := safeJoin(dst, zf.Name)
		if err != nil {
			return count, err
		}
		if zf.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return count, err
			}
			continue
		}
		if !zf.FileInfo().Mode().IsRegular() {
			slog.Warn("skipping unsupported archive entry", "name", zf.Name)
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			return count, err
		}
		err = writeEntry(target, zf.FileInfo().Mode(), rc)
		rc.Close()
		if err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

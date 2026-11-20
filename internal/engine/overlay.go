package engine

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// overlay is step 3: copy overlay_dir over the checkout root.
func (rc *runCtxt) overlay() error {
	dir := rc.cfg.OverlayDir
	if dir == "" {
		return nil
	}
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(rc.log, "\n==> [overlay] %s does not exist, skipped\n", dir)
		return nil
	}
	fmt.Fprintf(rc.log, "\n==> [overlay] copy %s (%s)\n", dir, stamp(timeNow()))
	n, err := copyOverlay(dir, rc.checkout, rc.log)
	if err != nil {
		fmt.Fprintf(rc.log, "overlay: %v\n", err)
		return stepErr("overlay", "copying overlay failed")
	}
	fmt.Fprintf(rc.log, "<== [overlay] %d files copied\n", n)
	return nil
}

// copyOverlay copies every regular file and directory under src over dst,
// keeping permission bits. Symlinks and special files are skipped with a
// warning written to warn. Writes go through an os.Root on dst, so a symlink
// in the checkout cannot redirect them outside of it. It returns the number of
// files copied.
func copyOverlay(src, dst string, warn io.Writer) (int, error) {
	root, err := os.OpenRoot(dst)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	files := 0
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode()
		switch {
		case rel == ".":
			return nil
		case mode&fs.ModeSymlink != 0:
			fmt.Fprintf(warn, "warning: overlay: skipping symlink %s\n", rel)
			return nil
		case mode.IsDir():
			if err := root.MkdirAll(rel, 0o755); err != nil {
				return err
			}
			return root.Chmod(rel, mode.Perm())
		case mode.IsRegular():
			if err := copyFileInto(root, rel, p, mode.Perm()); err != nil {
				return err
			}
			files++
			return nil
		default:
			fmt.Fprintf(warn, "warning: overlay: skipping special file %s\n", rel)
			return nil
		}
	})
	return files, err
}

func copyFileInto(root *os.Root, rel, src string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if d := filepath.Dir(rel); d != "." {
		if err := root.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	out, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return root.Chmod(rel, perm)
}

package engine

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/timothywashburn/ansibled/internal/model"
)

var shaRe = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

// fetchRef is the ref a run's commit is fetched from.
func (rc *runCtxt) fetchRef() string {
	if rc.run.Kind == model.KindMR {
		return rc.src.MRRef(rc.run.MRIID)
	}
	return "refs/heads/" + rc.run.Ref
}

// gitEnv is the environment for git: the base env plus, if header is set, an
// http.extraHeader passed through GIT_CONFIG_* (never argv).
func (rc *runCtxt) gitEnv(header string) *env {
	ev := rc.baseEnv("")
	ev.Set("GIT_TERMINAL_PROMPT", "0")
	ev.Set("GIT_CONFIG_NOSYSTEM", "1")
	if header != "" {
		ev.Set("GIT_CONFIG_COUNT", "1")
		ev.Set("GIT_CONFIG_KEY_0", "http.extraHeader")
		ev.Set("GIT_CONFIG_VALUE_0", "Authorization: "+header)
	}
	return ev
}

// fetch is step 1: update data_dir/repo.git and check the commit exists.
func (rc *runCtxt) fetch() error {
	sha := rc.run.SHA
	if !shaRe.MatchString(sha) {
		return stepErr("fetch", "invalid commit sha %q", sha)
	}
	ref := rc.fetchRef()
	if ref == "refs/heads/" || strings.HasPrefix(ref, "-") || strings.ContainsAny(ref, " \t\n:") {
		return stepErr("fetch", "invalid ref %q", ref)
	}
	fetchURL, header := rc.cfg.Git.CloneURL, ""
	if fetchURL == "" {
		fetchURL, header = rc.src.GitAuth()
	}
	if fetchURL == "" {
		return stepErr("fetch", "no fetch URL")
	}
	repo := rc.repoDir()
	ev := rc.gitEnv("")
	if _, err := os.Stat(repo + "/HEAD"); err != nil {
		if err := os.RemoveAll(repo); err != nil {
			return stepErr("fetch", "cannot reset repo.git")
		}
		if _, err := rc.exec(step{name: "fetch", argv: []string{"git", "init", "--quiet", "--bare", repo}, env: ev}); err != nil {
			return stepErr("fetch", "git init failed")
		}
	}
	authEnv := rc.gitEnv(header)
	argv := []string{"git", "-C", repo, "fetch", "--no-tags", "--force", "--", fetchURL, "+" + ref + ":" + ref}
	display := fmt.Sprintf("git -C %s fetch --no-tags --force -- %s +%s:%s", repo, redactURL(fetchURL), ref, ref)
	if _, err := rc.exec(step{name: "fetch", display: display, argv: argv, env: authEnv}); err != nil {
		if rc.ctx.Err() != nil {
			return rc.ctx.Err()
		}
		return stepErr("fetch", "git fetch %s failed", ref)
	}
	if _, err := rc.exec(step{name: "fetch", argv: []string{"git", "-C", repo, "cat-file", "-e", sha + "^{commit}"}, env: ev}); err != nil {
		if rc.ctx.Err() != nil {
			return rc.ctx.Err()
		}
		return stepErr("fetch", "commit %s not found after fetching %s", model.Short(sha), ref)
	}
	return nil
}

// redactURL drops any userinfo from a URL before it is logged.
func redactURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return s
	}
	u.User = nil
	return u.String()
}

// checkoutTree is step 2: git archive <sha> extracted into runs/<id>/checkout.
func (rc *runCtxt) checkoutTree() error {
	if err := os.RemoveAll(rc.checkout); err != nil {
		return stepErr("checkout", "cannot clear checkout directory")
	}
	if err := os.MkdirAll(rc.checkout, 0o755); err != nil {
		return stepErr("checkout", "cannot create checkout directory")
	}
	pr, pw := io.Pipe()
	extractErr := make(chan error, 1)
	go func() {
		err := extractTar(pr, rc.checkout)
		// Drain so git archive never blocks on a full pipe after an error.
		_, _ = io.Copy(io.Discard, pr)
		extractErr <- err
	}()
	_, err := rc.exec(step{
		name:   "checkout",
		argv:   []string{"git", "-C", rc.repoDir(), "archive", "--format=tar", rc.run.SHA},
		env:    rc.gitEnv(""),
		stdout: pw,
	})
	pw.CloseWithError(err)
	xerr := <-extractErr
	if rc.ctx.Err() != nil {
		return rc.ctx.Err()
	}
	if err != nil {
		return stepErr("checkout", "git archive failed")
	}
	if xerr != nil {
		fmt.Fprintf(rc.log, "extract: %v\n", xerr)
		return stepErr("checkout", "extracting archive: %v", xerr)
	}
	return nil
}

// errUnsafePath is returned for archive entries that would land outside the
// destination.
var errUnsafePath = errors.New("unsafe path in archive")

// safeRel validates an archive entry name and returns it cleaned and relative.
func safeRel(name string) (string, error) {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
		return "", fmt.Errorf("%w: %q", errUnsafePath, name)
	}
	c := path.Clean(name)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("%w: %q", errUnsafePath, name)
	}
	return c, nil
}

// extractTar extracts a tar stream (as produced by git archive) into dir.
// Directories, regular files and symlinks are supported; every operation goes
// through an os.Root, so no entry, also not one written through an earlier
// symlink, can escape dir.
func extractTar(r io.Reader, dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeXGlobalHeader, tar.TypeXHeader:
			continue
		}
		name, err := safeRel(h.Name)
		if err != nil {
			return err
		}
		if name == "." {
			continue
		}
		mode := fs.FileMode(h.Mode).Perm()
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o755); err != nil {
				return err
			}
			if err := root.Chmod(name, mode|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := mkParent(root, name); err != nil {
				return err
			}
			f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			_, cerr := io.Copy(f, tr)
			if err := f.Close(); cerr == nil {
				cerr = err
			}
			if cerr != nil {
				return cerr
			}
			if err := root.Chmod(name, mode); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := mkParent(root, name); err != nil {
				return err
			}
			if err := root.Symlink(h.Linkname, name); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported archive entry type %q for %q", h.Typeflag, h.Name)
		}
	}
}

func mkParent(root *os.Root, name string) error {
	if d := path.Dir(name); d != "." {
		return root.MkdirAll(d, 0o755)
	}
	return nil
}

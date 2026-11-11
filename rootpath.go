package archive

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// errPathEscapes is returned when resolving a path within the extraction root
// would leave the root through a relative symbolic link. It matches the error
// returned by [os.Root] (go1.24 and up) for the same condition.
var errPathEscapes = errors.New("path escapes from parent")

// errTooManyLinks is returned when resolving a path within the extraction root
// encounters too many symbolic links, for example because of a symlink loop.
var errTooManyLinks = errors.New("too many links")

// maxSymlinks is the maximum number of symbolic links that are followed when
// resolving a single path within the extraction root.
const maxSymlinks = 255

// isPathEscapes reports whether err is caused by a path that escapes the
// extraction root (see [resolveInRoot]).
func isPathEscapes(err error) bool {
	return errors.Is(err, errPathEscapes)
}

// resolveInRoot resolves name, a root-relative path in native (host-separator)
// form, to a root-relative path that contains no symbolic links. Every
// symbolic link that is encountered, including the final path component, is
// evaluated within root:
//
//   - Absolute symlink targets are evaluated relative to root (chroot-like
//     semantics), as is common for container root filesystems; from then on,
//     ".." components are bounded at root.
//   - A relative symlink that would resolve to a location above root is
//     rejected with an error for which [isPathEscapes] returns true.
//
// Components that do not exist are retained so that callers can create them.
// The returned path is always local to root, and is "." for root itself.
//
// The path is resolved before it is used, so it protects against symbolic
// links in the destination, including links created by earlier entries in
// the archive being extracted, but not against a concurrent modification of
// the destination by another process.
func resolveInRoot(root, name string) (string, error) {
	var (
		resolved    []string
		pending     = splitPath(name)
		linksWalked int
		followedAbs bool
	)
	for len(pending) > 0 {
		c := pending[0]
		pending = pending[1:]

		switch c {
		case ".":
			continue
		case "..":
			if len(resolved) > 0 {
				resolved = resolved[:len(resolved)-1]
			} else if !followedAbs {
				return "", &os.PathError{Op: "resolve", Path: name, Err: errPathEscapes}
			}
			continue
		}

		cur := filepath.Join(root, filepath.Join(resolved...), c)
		fi, err := os.Lstat(cur)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// Path does not (yet) exist; it cannot be a symlink.
				resolved = append(resolved, c)
				continue
			}
			return "", err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			resolved = append(resolved, c)
			continue
		}

		linksWalked++
		if linksWalked > maxSymlinks {
			return "", &os.PathError{Op: "resolve", Path: name, Err: errTooManyLinks}
		}
		target, err := os.Readlink(cur)
		if err != nil {
			return "", err
		}
		if vol := filepath.VolumeName(target); vol != "" || (target != "" && os.IsPathSeparator(target[0])) {
			// Absolute symlink: evaluate it relative to root.
			followedAbs = true
			resolved = resolved[:0]
			target = target[len(vol):]
		}
		pending = append(splitPath(target), pending...)
	}
	if len(resolved) == 0 {
		return ".", nil
	}
	return filepath.Join(resolved...), nil
}

// splitPath splits p into its non-empty components, using the path
// separator(s) of the host platform.
func splitPath(p string) []string {
	return strings.FieldsFunc(p, func(r rune) bool {
		return r < utf8.RuneSelf && os.IsPathSeparator(uint8(r))
	})
}

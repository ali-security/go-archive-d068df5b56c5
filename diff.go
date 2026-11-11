package archive

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/containerd/log"

	"github.com/moby/go-archive/compression"
)

// UnpackLayer unpack `layer` to a `dest`. The stream `layer` can be
// compressed or uncompressed.
// Returns the size in bytes of the contents of the layer.
func UnpackLayer(dest string, layer io.Reader, options *TarOptions) (size int64, err error) {
	tr := tar.NewReader(layer)

	var dirs []unpackedDir
	// unpackedPaths tracks the resolved paths already written in this layer
	// so that the AUFS opaque-whiteout walk knows which paths to preserve.
	unpackedPaths := make(map[string]struct{})

	if options == nil {
		options = &TarOptions{}
	}
	if options.ExcludePatterns == nil {
		options.ExcludePatterns = []string{}
	}

	aufsTempdir := ""
	aufsHardlinks := make(map[string]*tar.Header)

	// Iterate through the files in the archive.
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			// end of tar archive
			break
		}
		if err != nil {
			return 0, err
		}

		size += hdr.Size

		// Strip a leading "/" so absolute entries stay root-relative, and
		// normalize the POSIX tar path. Only a directory can be merged with
		// the extraction root, and paths that escape it are rejected.
		name := path.Clean(strings.TrimLeft(hdr.Name, "/"))
		if name == "." && hdr.Typeflag != tar.TypeDir {
			continue
		}

		// Windows does not support filenames with colons in them. Ignore
		// these files. This is not a problem though (although it might
		// appear that it is). Let's suppose a client is running docker pull.
		// The daemon it points to is Windows. Would it make sense for the
		// client to be doing a docker pull Ubuntu for example (which has files
		// with colons in the name under /usr/share/man/man3)? No, absolutely
		// not as it would really only make sense that they were pulling a
		// Windows image. However, for development, it is necessary to be able
		// to pull Linux images which are in the repository.
		//
		// TODO Windows. Once the registry is aware of what images are Windows-
		// specific or Linux-specific, this warning should be changed to an error
		// to cater for the situation where someone does manage to upload a Linux
		// image but have it tagged as Windows inadvertently.
		if err := unrepresentableOnWindows(name, hdr); err != nil {
			log.G(context.TODO()).Warnf("Windows: ignoring entry: %v", err)
			continue
		}

		if !filepath.IsLocal(name) {
			return 0, breakoutError(fmt.Errorf("invalid entry name %q", hdr.Name))
		}
		hdr.Name = name

		// Skip AUFS metadata dirs
		if strings.HasPrefix(hdr.Name, WhiteoutMetaPrefix) {
			// Regular files inside /.wh..wh.plnk can be used as hardlink targets
			// We don't want this directory, but we need the files in them so that
			// such hardlinks can be resolved.
			if strings.HasPrefix(hdr.Name, WhiteoutLinkDir) && hdr.Typeflag == tar.TypeReg {
				basename := path.Base(hdr.Name)
				aufsHardlinks[basename] = hdr
				if aufsTempdir == "" {
					if aufsTempdir, err = os.MkdirTemp(dest, "dockerplnk"); err != nil {
						return 0, err
					}
					defer os.RemoveAll(aufsTempdir)
				}
				if err := createTarFile(filepath.Join(aufsTempdir, basename), dest, hdr, tr, options); err != nil {
					return 0, err
				}
			}

			if hdr.Name != WhiteoutOpaqueDir {
				continue
			}
		}

		// Resolve symlinks in the parent directories of the entry within
		// dest, so that symlinks in the destination (including symlinks
		// created by earlier entries in the layer) cannot be used to write
		// outside of it. relPath is in native (host-separator) form.
		relPath, err := resolveArchivePath(dest, filepath.FromSlash(hdr.Name))
		if err != nil {
			return 0, err
		}

		// Ensure that the parent directory exists.
		if err := createImpliedDirectories(dest, relPath, options); err != nil {
			return 0, err
		}

		dstPath := filepath.Join(dest, relPath)
		base := filepath.Base(dstPath)

		if strings.HasPrefix(base, WhiteoutPrefix) {
			dir := filepath.Dir(dstPath)
			if base == WhiteoutOpaqueDir {
				_, err := os.Lstat(dir)
				if err != nil {
					return 0, err
				}
				err = filepath.WalkDir(dir, func(p string, info os.DirEntry, err error) error {
					if err != nil {
						if os.IsNotExist(err) {
							err = nil // parent was deleted
						}
						return err
					}
					if p == dir {
						return nil
					}
					if _, exists := unpackedPaths[p]; !exists {
						return os.RemoveAll(p)
					}
					return nil
				})
				if err != nil {
					return 0, err
				}
			} else {
				originalBase := base[len(WhiteoutPrefix):]
				originalPath := filepath.Join(dir, originalBase)
				if err := os.RemoveAll(originalPath); err != nil {
					return 0, err
				}
			}
		} else {
			// If dstPath exists we almost always just want to remove and replace it.
			// The only exception is when it is a directory *and* the file from
			// the layer is also a directory. Then we want to merge them (i.e.
			// just apply the metadata from the layer).
			if fi, err := os.Lstat(dstPath); err == nil {
				if !fi.IsDir() || hdr.Typeflag != tar.TypeDir {
					if err := os.RemoveAll(dstPath); err != nil {
						return 0, err
					}
				}
			}

			srcData := io.Reader(tr)
			srcHdr := hdr

			// Hard links into /.wh..wh.plnk don't work, as we don't extract that directory, so
			// we manually retarget these into the temporary files we extracted them into
			if hdr.Typeflag == tar.TypeLink && strings.HasPrefix(path.Clean(hdr.Linkname), WhiteoutLinkDir) {
				linkBasename := path.Base(hdr.Linkname)
				srcHdr = aufsHardlinks[linkBasename]
				if srcHdr == nil {
					return 0, errors.New("invalid aufs hardlink")
				}
				tmpFile, err := os.Open(filepath.Join(aufsTempdir, linkBasename))
				if err != nil {
					return 0, err
				}
				defer tmpFile.Close()
				srcData = tmpFile
			}

			if err := remapIDs(options.IDMap, srcHdr); err != nil {
				return 0, err
			}

			if err := createTarFile(dstPath, dest, srcHdr, srcData, options); err != nil {
				return 0, err
			}

			// Directory mtimes must be handled at the end to avoid further
			// file creation in them to modify the directory mtime
			if hdr.Typeflag == tar.TypeDir {
				dirs = append(dirs, unpackedDir{hdr: hdr, name: relPath})
			}
			unpackedPaths[dstPath] = struct{}{}
		}
	}

	for _, d := range dirs {
		// Resolve the directory within dest again, as entries extracted
		// after it may have replaced it (or its parents) with a symlink.
		name, err := resolveInRoot(dest, d.name)
		if err != nil {
			return 0, err
		}
		dstPath := filepath.Join(dest, name)
		if err := chtimes(dstPath, d.hdr.AccessTime, d.hdr.ModTime); err != nil {
			return 0, err
		}
	}

	return size, nil
}

// ApplyLayer parses a diff in the standard layer format from `layer`,
// and applies it to the directory `dest`. The stream `layer` can be
// compressed or uncompressed.
// Returns the size in bytes of the contents of the layer.
func ApplyLayer(dest string, layer io.Reader) (int64, error) {
	return applyLayerHandler(dest, layer, &TarOptions{}, true)
}

// ApplyUncompressedLayer parses a diff in the standard layer format from
// `layer`, and applies it to the directory `dest`. The stream `layer`
// can only be uncompressed.
// Returns the size in bytes of the contents of the layer.
func ApplyUncompressedLayer(dest string, layer io.Reader, options *TarOptions) (int64, error) {
	return applyLayerHandler(dest, layer, options, false)
}

// IsEmpty checks if the tar archive is empty (doesn't contain any entries).
func IsEmpty(rd io.Reader) (bool, error) {
	decompRd, err := compression.DecompressStream(rd)
	if err != nil {
		return true, fmt.Errorf("failed to decompress archive: %w", err)
	}
	defer decompRd.Close()

	tarReader := tar.NewReader(decompRd)
	if _, err := tarReader.Next(); err != nil {
		if errors.Is(err, io.EOF) {
			return true, nil
		}
		return false, fmt.Errorf("failed to read next archive header: %w", err)
	}

	return false, nil
}

// do the bulk load of ApplyLayer, but allow for not calling DecompressStream
func applyLayerHandler(dest string, layer io.Reader, options *TarOptions, decompress bool) (int64, error) {
	dest = filepath.Clean(dest)

	// We need to be able to set any perms
	restore := overrideUmask(0)
	defer restore()

	if decompress {
		decompLayer, err := compression.DecompressStream(layer)
		if err != nil {
			return 0, err
		}
		defer decompLayer.Close()
		layer = decompLayer
	}
	return UnpackLayer(dest, layer, options)
}

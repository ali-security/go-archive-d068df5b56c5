//go:build !windows

package archive

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/moby/sys/userns"
	"golang.org/x/sys/unix"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/skip"

	"github.com/moby/go-archive/compression"
)

func TestCanonicalTarName(t *testing.T) {
	cases := []struct {
		in       string
		isDir    bool
		expected string
	}{
		{"foo", false, "foo"},
		{"foo", true, "foo/"},
		{"foo/bar", false, "foo/bar"},
		{"foo/bar", true, "foo/bar/"},
	}
	for _, v := range cases {
		if canonicalTarName(v.in, v.isDir) != v.expected {
			t.Fatalf("wrong canonical tar name. expected:%s got:%s", v.expected, canonicalTarName(v.in, v.isDir))
		}
	}
}

func TestChmodTarEntry(t *testing.T) {
	cases := []struct {
		in, expected os.FileMode
	}{
		{0o000, 0o000},
		{0o777, 0o777},
		{0o644, 0o644},
		{0o755, 0o755},
		{0o444, 0o444},
	}
	for _, v := range cases {
		if out := chmodTarEntry(v.in); out != v.expected {
			t.Fatalf("wrong chmod. expected:%v got:%v", v.expected, out)
		}
	}
}

func TestTarWithHardLink(t *testing.T) {
	origin, err := os.MkdirTemp("", "docker-test-tar-hardlink")
	assert.NilError(t, err)
	defer os.RemoveAll(origin)

	err = os.WriteFile(filepath.Join(origin, "1"), []byte("hello world"), 0o700)
	assert.NilError(t, err)

	err = os.Link(filepath.Join(origin, "1"), filepath.Join(origin, "2"))
	assert.NilError(t, err)

	var i1, i2 uint64
	i1, err = getNlink(filepath.Join(origin, "1"))
	assert.NilError(t, err)

	// sanity check that we can hardlink
	if i1 != 2 {
		t.Skipf("skipping since hardlinks don't work here; expected 2 links, got %d", i1)
	}

	dest, err := os.MkdirTemp("", "docker-test-tar-hardlink-dest")
	assert.NilError(t, err)
	defer os.RemoveAll(dest)

	// we'll do this in two steps to separate failure
	fh, err := Tar(origin, compression.None)
	assert.NilError(t, err)

	// ensure we can read the whole thing with no error, before writing back out
	buf, err := io.ReadAll(fh)
	assert.NilError(t, err)

	bRdr := bytes.NewReader(buf)
	err = Untar(bRdr, dest, nil)
	assert.NilError(t, err)

	i1, err = getInode(filepath.Join(dest, "1"))
	assert.NilError(t, err)

	i2, err = getInode(filepath.Join(dest, "2"))
	assert.NilError(t, err)

	assert.Check(t, is.Equal(i1, i2))
}

func TestTarWithHardLinkAndRebase(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "docker-test-tar-hardlink-rebase")
	assert.NilError(t, err)
	defer os.RemoveAll(tmpDir)

	origin := filepath.Join(tmpDir, "origin")
	err = os.Mkdir(origin, 0o700)
	assert.NilError(t, err)

	err = os.WriteFile(filepath.Join(origin, "1"), []byte("hello world"), 0o700)
	assert.NilError(t, err)

	err = os.Link(filepath.Join(origin, "1"), filepath.Join(origin, "2"))
	assert.NilError(t, err)

	var i1, i2 uint64
	i1, err = getNlink(filepath.Join(origin, "1"))
	assert.NilError(t, err)

	// sanity check that we can hardlink
	if i1 != 2 {
		t.Skipf("skipping since hardlinks don't work here; expected 2 links, got %d", i1)
	}

	dest := filepath.Join(tmpDir, "dest")
	bRdr, err := TarResourceRebase(origin, "origin")
	assert.NilError(t, err)

	dstDir, srcBase := SplitPathDirEntry(origin)
	_, dstBase := SplitPathDirEntry(dest)
	content := RebaseArchiveEntries(bRdr, srcBase, dstBase)
	err = Untar(content, dstDir, &TarOptions{NoLchown: true, NoOverwriteDirNonDir: true})
	assert.NilError(t, err)

	i1, err = getInode(filepath.Join(dest, "1"))
	assert.NilError(t, err)
	i2, err = getInode(filepath.Join(dest, "2"))
	assert.NilError(t, err)

	assert.Check(t, is.Equal(i1, i2))
}

// TestUntarParentPathPermissions is a regression test to check that missing
// parent directories are created with the expected permissions
func TestUntarParentPathPermissions(t *testing.T) {
	skip.If(t, os.Getuid() != 0, "skipping test that requires root")
	buf := &bytes.Buffer{}
	w := tar.NewWriter(buf)
	err := w.WriteHeader(&tar.Header{Name: "foo/bar"})
	assert.NilError(t, err)
	tmpDir, err := os.MkdirTemp("", t.Name())
	assert.NilError(t, err)
	defer os.RemoveAll(tmpDir)
	err = Untar(buf, tmpDir, nil)
	assert.NilError(t, err)

	fi, err := os.Lstat(filepath.Join(tmpDir, "foo"))
	assert.NilError(t, err)
	assert.Equal(t, fi.Mode(), 0o755|os.ModeDir)
}

func getNlink(path string) (uint64, error) {
	stat, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	statT, ok := stat.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("expected type *syscall.Stat_t, got %t", stat.Sys())
	}
	// We need this conversion on ARM64
	//nolint: unconvert
	return uint64(statT.Nlink), nil
}

func getInode(path string) (uint64, error) {
	stat, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	statT, ok := stat.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("expected type *syscall.Stat_t, got %t", stat.Sys())
	}
	return statT.Ino, nil
}

func TestTarWithBlockCharFifo(t *testing.T) {
	skip.If(t, os.Getuid() != 0, "skipping test that requires root")
	skip.If(t, userns.RunningInUserNS(), "skipping test that requires initial userns")
	origin, err := os.MkdirTemp("", "docker-test-tar-hardlink")
	assert.NilError(t, err)

	defer os.RemoveAll(origin)
	err = os.WriteFile(filepath.Join(origin, "1"), []byte("hello world"), 0o700)
	assert.NilError(t, err)

	err = mknod(filepath.Join(origin, "2"), unix.S_IFBLK, unix.Mkdev(uint32(12), uint32(5)))
	assert.NilError(t, err)
	err = mknod(filepath.Join(origin, "3"), unix.S_IFCHR, unix.Mkdev(uint32(12), uint32(5)))
	assert.NilError(t, err)
	err = mknod(filepath.Join(origin, "4"), unix.S_IFIFO, unix.Mkdev(uint32(12), uint32(5)))
	assert.NilError(t, err)

	dest, err := os.MkdirTemp("", "docker-test-tar-hardlink-dest")
	assert.NilError(t, err)
	defer os.RemoveAll(dest)

	// we'll do this in two steps to separate failure
	fh, err := Tar(origin, compression.None)
	assert.NilError(t, err)

	// ensure we can read the whole thing with no error, before writing back out
	buf, err := io.ReadAll(fh)
	assert.NilError(t, err)

	bRdr := bytes.NewReader(buf)
	err = Untar(bRdr, dest, nil)
	assert.NilError(t, err)

	changes, err := ChangesDirs(origin, dest)
	assert.NilError(t, err)

	if len(changes) > 0 {
		t.Fatalf("Tar with special device (block, char, fifo) should keep them (recreate them when untar) : %v", changes)
	}
}

// TestTarUntarWithXattr is Unix as Lsetxattr is not supported on Windows
func TestTarUntarWithXattr(t *testing.T) {
	skip.If(t, os.Getuid() != 0, "skipping test that requires root")
	if _, err := exec.LookPath("setcap"); err != nil {
		t.Skip("setcap not installed")
	}
	if _, err := exec.LookPath("getcap"); err != nil {
		t.Skip("getcap not installed")
	}

	origin, err := os.MkdirTemp("", "docker-test-untar-origin")
	assert.NilError(t, err)
	defer os.RemoveAll(origin)
	err = os.WriteFile(filepath.Join(origin, "1"), []byte("hello world"), 0o700)
	assert.NilError(t, err)

	err = os.WriteFile(filepath.Join(origin, "2"), []byte("welcome!"), 0o700)
	assert.NilError(t, err)
	err = os.WriteFile(filepath.Join(origin, "3"), []byte("will be ignored"), 0o700)
	assert.NilError(t, err)
	// there is no known Go implementation of setcap/getcap with support for v3 file capability
	out, err := exec.Command("setcap", "cap_block_suspend+ep", filepath.Join(origin, "2")).CombinedOutput()
	assert.NilError(t, err, string(out))

	tarball, err := Tar(origin, compression.None)
	assert.NilError(t, err)
	defer tarball.Close()
	rdr := tar.NewReader(tarball)
	for {
		h, err := rdr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		assert.NilError(t, err)
		capability, hasxattr := h.PAXRecords["SCHILY.xattr.security.capability"]
		switch h.Name {
		case "2":
			if assert.Check(t, hasxattr, "tar entry %q should have the 'security.capability' xattr", h.Name) {
				assert.Check(t, len(capability) > 0, "tar entry %q has a blank 'security.capability' xattr value")
			}
		default:
			assert.Check(t, !hasxattr, "tar entry %q should not have the 'security.capability' xattr", h.Name)
		}
	}

	for _, c := range []compression.Compression{
		compression.None,
		compression.Gzip,
	} {
		changes, err := tarUntar(t, origin, &TarOptions{
			Compression:     c,
			ExcludePatterns: []string{"3"},
		})
		if err != nil {
			t.Fatalf("Error tar/untar for compression %s: %s", c.Extension(), err)
		}

		if len(changes) != 1 || changes[0].Path != "/3" {
			t.Fatalf("Unexpected differences after tarUntar: %v", changes)
		}
		out, err := exec.Command("getcap", filepath.Join(origin, "2")).CombinedOutput()
		assert.NilError(t, err, string(out))
		assert.Check(t, is.Contains(string(out), "cap_block_suspend=ep"), "untar should have kept the 'security.capability' xattr")
	}
}

func TestCopyInfoDestinationPathSymlink(t *testing.T) {
	tmpDir, _ := getTestTempDirs(t)
	defer removeAllPaths(tmpDir)

	root := strings.TrimRight(tmpDir, "/") + "/"

	type FileTestData struct {
		resource FileData
		file     string
		expected CopyInfo
	}

	testData := []FileTestData{
		// Create a directory: /tmp/archive-copy-test*/dir1
		// Test will "copy" file1 to dir1
		{resource: FileData{filetype: Dir, path: "dir1", permissions: 0o740}, file: "file1", expected: CopyInfo{Path: root + "dir1/file1", Exists: false, IsDir: false}},

		// Create a symlink directory to dir1: /tmp/archive-copy-test*/dirSymlink -> dir1
		// Test will "copy" file2 to dirSymlink
		{resource: FileData{filetype: Symlink, path: "dirSymlink", contents: root + "dir1", permissions: 0o600}, file: "file2", expected: CopyInfo{Path: root + "dirSymlink/file2", Exists: false, IsDir: false}},

		// Create a file in tmp directory: /tmp/archive-copy-test*/file1
		// Test to cover when the full file path already exists.
		{resource: FileData{filetype: Regular, path: "file1", permissions: 0o600}, file: "", expected: CopyInfo{Path: root + "file1", Exists: true}},

		// Create a directory: /tmp/archive-copy*/dir2
		// Test to cover when the full directory path already exists
		{resource: FileData{filetype: Dir, path: "dir2", permissions: 0o740}, file: "", expected: CopyInfo{Path: root + "dir2", Exists: true, IsDir: true}},

		// Create a symlink to a non-existent target: /tmp/archive-copy*/symlink1 -> noSuchTarget
		// Negative test to cover symlinking to a target that does not exit
		{resource: FileData{filetype: Symlink, path: "symlink1", contents: "noSuchTarget", permissions: 0o600}, file: "", expected: CopyInfo{Path: root + "noSuchTarget", Exists: false}},

		// Create a file in tmp directory for next test: /tmp/existingfile
		{resource: FileData{filetype: Regular, path: "existingfile", permissions: 0o600}, file: "", expected: CopyInfo{Path: root + "existingfile", Exists: true}},

		// Create a symlink to an existing file: /tmp/archive-copy*/symlink2 -> /tmp/existingfile
		// Test to cover when the parent directory of a new file is a symlink
		{resource: FileData{filetype: Symlink, path: "symlink2", contents: "existingfile", permissions: 0o600}, file: "", expected: CopyInfo{Path: root + "existingfile", Exists: true}},
	}

	var dirs []FileData
	for _, data := range testData {
		dirs = append(dirs, data.resource)
	}
	provisionSampleDir(t, tmpDir, dirs)

	for _, info := range testData {
		p := filepath.Join(tmpDir, info.resource.path, info.file)
		ci, err := CopyInfoDestinationPath(p)
		assert.Check(t, err)
		assert.Check(t, is.DeepEqual(info.expected, ci))
	}
}

// TestUntarThroughAbsoluteSymlink verifies that archive extraction follows a
// pre-existing absolute symlink relative to the extraction root, including
// when the symlink target or directories following it do not yet exist.
//
// Regression test for https://github.com/moby/moby/issues/53258
func TestUntarThroughAbsoluteSymlink(t *testing.T) {
	unpackers := []struct {
		name   string
		unpack func(dest string, r io.Reader) error
	}{
		{
			name: "Untar",
			unpack: func(dest string, r io.Reader) error {
				return Untar(r, dest, &TarOptions{NoLchown: true})
			},
		},
		{
			name: "UnpackLayer",
			unpack: func(dest string, r io.Reader) error {
				_, err := UnpackLayer(dest, r, &TarOptions{NoLchown: true})
				return err
			},
		},
	}

	for _, unpacker := range unpackers {
		t.Run(unpacker.name, func(t *testing.T) {
			for _, tc := range []struct {
				name         string
				createTarget bool
			}{
				{
					name:         "existing target",
					createTarget: true,
				},
				{
					name:         "missing target",
					createTarget: false,
				},
			} {
				t.Run(tc.name, func(t *testing.T) {
					const (
						name    = "var/run/existing/non-existing/file"
						content = "content"
					)

					dest := t.TempDir()
					assert.NilError(t, os.Mkdir(filepath.Join(dest, "var"), 0o755))
					if tc.createTarget {
						assert.NilError(t, os.MkdirAll(
							filepath.Join(dest, "run", "existing"),
							0o755,
						))
					}
					assert.NilError(t, os.Symlink(
						"/run",
						filepath.Join(dest, "var", "run"),
					))

					buf := &bytes.Buffer{}
					tw := tar.NewWriter(buf)
					assert.NilError(t, tw.WriteHeader(&tar.Header{
						Name:     name,
						Typeflag: tar.TypeReg,
						Mode:     0o644,
						Size:     int64(len(content)),
					}))
					_, err := io.WriteString(tw, content)
					assert.NilError(t, err)
					assert.NilError(t, tw.Close())

					assert.NilError(t, unpacker.unpack(dest, buf))

					actual, err := os.ReadFile(filepath.Join(
						dest, "run", "existing", "non-existing", "file",
					))
					assert.NilError(t, err)
					assert.DeepEqual(t, actual, []byte(content))
				})
			}
		})
	}
}

// A relative symlink must not escape the extraction root merely because path
// resolution encounters an absolute symlink afterward.
func TestUnpackRejectsRelativeEscapeBeforeAbsoluteSymlink(t *testing.T) {
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	assert.NilError(t, tw.WriteHeader(&tar.Header{
		Name:     "escape/absolute/file",
		Typeflag: tar.TypeReg,
		Mode:     0o644,
	}))
	assert.NilError(t, tw.Close())

	unpackers := []struct {
		name   string
		unpack func(dest string, r io.Reader) error
	}{
		{
			name: "Unpack",
			unpack: func(dest string, r io.Reader) error {
				return Unpack(r, dest, &TarOptions{NoLchown: true})
			},
		},
		{
			name: "UnpackLayer",
			unpack: func(dest string, r io.Reader) error {
				_, err := UnpackLayer(dest, r, &TarOptions{NoLchown: true})
				return err
			},
		},
	}

	for _, unpacker := range unpackers {
		t.Run(unpacker.name, func(t *testing.T) {
			dest := t.TempDir()
			assert.NilError(t, os.Mkdir(filepath.Join(dest, "target"), 0o755))
			assert.NilError(t, os.Symlink("..", filepath.Join(dest, "escape")))
			assert.NilError(t, os.Symlink(
				"/target",
				filepath.Join(dest, "absolute"),
			))

			err := unpacker.unpack(dest, bytes.NewReader(buf.Bytes()))
			assert.Check(t, isPathEscapes(err), "expected path-escape error, got: %v", err)

			_, err = os.Lstat(filepath.Join(dest, "target", "file"))
			assert.Check(t, os.IsNotExist(err), "archive wrote through rejected path: %v", err)
		})
	}
}

// Absolute symlinks are common in container root filesystems and may come from
// a lower layer. Later layers must resolve files and hardlink sources through
// those symlinks relative to the extraction root, not the host root.
func TestHardlinkSourceThroughAbsoluteSymlink(t *testing.T) {
	const content = "content"

	unpackers := []struct {
		name   string
		unpack func(io.Reader, string) error
	}{
		{
			name: "Unpack",
			unpack: func(r io.Reader, dest string) error {
				return Unpack(r, dest, &TarOptions{NoLchown: true})
			},
		},
		{
			name: "UnpackLayer",
			unpack: func(r io.Reader, dest string) error {
				_, err := UnpackLayer(dest, r, &TarOptions{NoLchown: true})
				return err
			},
		},
	}

	for _, tc := range unpackers {
		t.Run(tc.name, func(t *testing.T) {
			dest := t.TempDir()
			assert.NilError(t, os.Mkdir(filepath.Join(dest, "var"), 0o755))
			assert.NilError(t, os.Symlink("/run", filepath.Join(dest, "var", "run")))

			buf := &bytes.Buffer{}
			tw := tar.NewWriter(buf)
			assert.NilError(t, tw.WriteHeader(&tar.Header{
				Name:     "var/run/source",
				Typeflag: tar.TypeReg,
				Mode:     0o644,
				Size:     int64(len(content)),
			}))
			_, err := io.WriteString(tw, content)
			assert.NilError(t, err)
			assert.NilError(t, tw.WriteHeader(&tar.Header{
				Name:     "var/run/link",
				Typeflag: tar.TypeLink,
				Linkname: "var/run/source",
				Mode:     0o644,
			}))
			assert.NilError(t, tw.Close())

			assert.NilError(t, tc.unpack(buf, dest))

			source := filepath.Join(dest, "run", "source")
			link := filepath.Join(dest, "run", "link")
			actual, err := os.ReadFile(link)
			assert.NilError(t, err)
			assert.DeepEqual(t, actual, []byte(content))

			sourceInode, err := getInode(source)
			assert.NilError(t, err)
			linkInode, err := getInode(link)
			assert.NilError(t, err)
			assert.Equal(t, sourceInode, linkInode)

			linkCount, err := getNlink(source)
			assert.NilError(t, err)
			assert.Equal(t, linkCount, uint64(2))
		})
	}
}

// TestUntarHardlinkThroughSymlinkChain verifies that a hardlink target cannot
// be redirected outside the extraction root through a chain of symlinks that
// each pass the static (lexical) symlink-target check, but escape the root
// when they are resolved by the OS. Without resolving the hardlink target
// within the extraction root, the hardlink would expose (and allow modifying)
// a file outside of it.
func TestUntarHardlinkThroughSymlinkChain(t *testing.T) {
	unpackers := []struct {
		name   string
		unpack func(dest string, r io.Reader) error
	}{
		{
			name: "Untar",
			unpack: func(dest string, r io.Reader) error {
				return Untar(r, dest, &TarOptions{NoLchown: true})
			},
		},
		{
			name: "UnpackLayer",
			unpack: func(dest string, r io.Reader) error {
				_, err := UnpackLayer(dest, r, &TarOptions{NoLchown: true})
				return err
			},
		},
	}

	for _, unpacker := range unpackers {
		t.Run(unpacker.name, func(t *testing.T) {
			tmpdir := t.TempDir()
			dest := filepath.Join(tmpdir, "dest")
			victim := filepath.Join(tmpdir, "victim")
			assert.NilError(t, os.Mkdir(dest, 0o755))
			assert.NilError(t, os.Mkdir(victim, 0o755))
			assert.NilError(t, os.WriteFile(filepath.Join(victim, "hello"), []byte("secret"), 0o600))

			buf := &bytes.Buffer{}
			tw := tar.NewWriter(buf)
			for _, hdr := range []*tar.Header{
				{Name: "inner", Typeflag: tar.TypeDir, Mode: 0o755},
				// "inner/.." is dest: passes the static check.
				{Name: "inner/go_up", Typeflag: tar.TypeSymlink, Linkname: ".."},
				// "inner/go_up/.." is lexically "inner": passes the static
				// check, but is the parent of dest when resolved by the OS.
				{Name: "escape", Typeflag: tar.TypeSymlink, Linkname: "inner/go_up/.."},
				{Name: "grab", Typeflag: tar.TypeLink, Linkname: "escape/victim/hello", Mode: 0o644},
			} {
				assert.NilError(t, tw.WriteHeader(hdr))
			}
			assert.NilError(t, tw.Close())

			err := unpacker.unpack(dest, buf)
			assert.Check(t, isPathEscapes(err), "expected path-escape error, got: %v", err)

			_, err = os.Lstat(filepath.Join(dest, "grab"))
			assert.Check(t, os.IsNotExist(err), "hardlink to a file outside the extraction root was created: %v", err)

			fi, err := os.Stat(filepath.Join(victim, "hello"))
			assert.NilError(t, err)
			assert.Check(t, is.Equal(fi.Mode().Perm(), os.FileMode(0o600)), "file outside the extraction root was modified")
		})
	}
}

// TestUntarDirectoryTimesThroughSymlinkChain verifies that restoring directory
// mtimes after extraction does not follow a symlink that replaced the
// directory to a location outside the extraction root.
func TestUntarDirectoryTimesThroughSymlinkChain(t *testing.T) {
	unpackers := []struct {
		name   string
		unpack func(dest string, r io.Reader) error
	}{
		{
			name: "Untar",
			unpack: func(dest string, r io.Reader) error {
				return Untar(r, dest, &TarOptions{NoLchown: true})
			},
		},
		{
			name: "UnpackLayer",
			unpack: func(dest string, r io.Reader) error {
				_, err := UnpackLayer(dest, r, &TarOptions{NoLchown: true})
				return err
			},
		},
	}

	past := time.Date(2001, time.January, 1, 0, 0, 0, 0, time.UTC)

	for _, unpacker := range unpackers {
		t.Run(unpacker.name, func(t *testing.T) {
			tmpdir := t.TempDir()
			dest := filepath.Join(tmpdir, "dest")
			victim := filepath.Join(tmpdir, "victim")
			assert.NilError(t, os.Mkdir(dest, 0o755))
			assert.NilError(t, os.Mkdir(victim, 0o755))

			buf := &bytes.Buffer{}
			tw := tar.NewWriter(buf)
			for _, hdr := range []*tar.Header{
				{Name: "inner", Typeflag: tar.TypeDir, Mode: 0o755},
				{Name: "inner/go_up", Typeflag: tar.TypeSymlink, Linkname: ".."},
				{Name: "dir", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: past},
				// Replace "dir" with a symlink that is lexically "inner/victim",
				// but is "../victim" when resolved by the OS.
				{Name: "dir", Typeflag: tar.TypeSymlink, Linkname: "inner/go_up/../victim"},
			} {
				assert.NilError(t, tw.WriteHeader(hdr))
			}
			assert.NilError(t, tw.Close())

			_ = unpacker.unpack(dest, buf) // may error; we only require containment

			fi, err := os.Stat(victim)
			assert.NilError(t, err)
			assert.Check(t, !fi.ModTime().Equal(past), "mtime of a directory outside the extraction root was modified")
		})
	}
}

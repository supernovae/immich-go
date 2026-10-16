package fshelper

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

//  VirtualGlobFS create a FS that limits the WalkDir function to the
//  list of files that match the glob expression, and cheats to
//  matches *.XMP files in all circumstances
//
//  It implements ReadDir and Stat to filter the file list
//

type VirtualGlobFS struct {
	rootFS NameFS
	parts  []string
}

var (
	_ fs.FS        = VirtualGlobFS{}
	_ fs.ReadDirFS = VirtualGlobFS{}
	_ fs.StatFS    = VirtualGlobFS{}
	_ NameFS       = VirtualGlobFS{}
)

func NewVirtualGlobFS(pattern string) (fs.FS, error) {
	rootFs, parts, err := getRootFs(pattern)
	if err != nil {
		return nil, err
	}

	return &VirtualGlobFS{
		rootFS: rootFs,
		parts:  parts,
	}, nil
}

func shouldSkipDir(err error) bool {
	if errors.Is(err, fs.ErrPermission) {
		return true
	}
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		if errors.Is(pathErr.Err, fs.ErrPermission) {
			return true
		}
		if errors.Is(pathErr.Err, syscall.EPERM) || errors.Is(pathErr.Err, syscall.EACCES) {
			return true
		}
	}
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		return true
	}
	return false
}

func getRootFs(pattern string) (NameFS, []string, error) {
	dir, magic := FixedPathAndMagic(pattern)
	if magic == "" {
		return simpleRootFS(dir)
	}

	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return nil, nil, err
		}
	} else if _, err := os.Stat(filepath.FromSlash(dir)); err != nil {
		return nil, nil, err
	}

	return NewFSWithName(dir), strings.Split(strings.ToLower(magic), "/"), nil
}

func simpleRootFS(root string) (NameFS, []string, error) {
	s, err := os.Stat(filepath.FromSlash(root))
	if err != nil {
		return nil, nil, err
	}

	if !s.IsDir() {
		// In the case of a file, we use the filename as the matching "magic" component.
		file := strings.ToLower(path.Base(root))
		rootDir := path.Dir(root)
		if rootDir == "" {
			rootDir, _ = os.Getwd()
		}
		return NewFSWithName(rootDir), []string{file}, nil
	}

	// Otherwise, we just use the directory
	return NewFSWithName(root), nil, nil
}

// match the current file name with the pattern
// matches files having a path starting by the patten
//
//	ex:  file /path/to/file matches with the pattern /*/to
func (gw VirtualGlobFS) match(filename string, isDir bool) bool {
	if filename == "." {
		return true
	}

	lowerFName := strings.ToLower(filename)
	if !isDir && strings.HasSuffix(lowerFName, ".xmp") {
		return true
	}

	if len(gw.parts) == 0 {
		return true
	}

	parts := strings.Split(lowerFName, "/")

	for i := range min(len(gw.parts), len(parts)) {
		if m, err := path.Match(gw.parts[i], parts[i]); err != nil || !m {
			return false
		}
	}

	if len(gw.parts) > len(parts) {
		return isDir
	}
	return true
}

// Open the name only if the name matches with the pattern
func (gw VirtualGlobFS) Open(name string) (fs.File, error) {
	f, err := gw.rootFS.Open(name)
	if err != nil {
		return nil, err
	}
	if len(gw.parts) > 0 && name != "." {
		fi, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		if !gw.match(name, fi.IsDir()) {
			_ = f.Close()
			return nil, fmt.Errorf("%s: %w", name, fs.ErrNotExist)
		}
	}
	return f, nil
}

// Stat the name only if the name matches with the pattern
func (gw VirtualGlobFS) Stat(name string) (fs.FileInfo, error) {
	fi, err := fs.Stat(gw.rootFS, name)
	if err != nil {
		return nil, err
	}
	if !gw.match(name, fi.IsDir()) {
		return nil, fmt.Errorf("%s: %w", name, fs.ErrNotExist)
	}
	return fi, nil
}

// ReadDir return all DirEntries that match with the pattern or .XMP files
func (gw VirtualGlobFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if !gw.match(name, true) {
		return nil, fmt.Errorf("%s: %w", name, fs.ErrNotExist)
	}
	rawEntries, err := fs.ReadDir(gw.rootFS, name)
	if err != nil {
		if shouldSkipDir(err) && name != "." {
			return nil, nil
		}
		return nil, err
	}
	if len(gw.parts) == 0 {
		return rawEntries, nil
	}

	entries := make([]fs.DirEntry, 0, len(rawEntries))
	for _, d := range rawEntries {
		full := path.Join(name, d.Name())
		if !gw.match(full, d.IsDir()) {
			continue
		}
		entries = append(entries, d)
	}
	return entries, nil
}

// Name gives the folder name when the argument was '.'
func (gw VirtualGlobFS) Name() string {
	return gw.rootFS.Name()
}

// FixedPathAndMagic split the path with the fixed part and the variable part
func FixedPathAndMagic(name string) (string, string) {
	if !HasMagic(name) {
		return name, ""
	}
	parts := strings.Split(name, "/")
	p := 0
	for p = range parts {
		if HasMagic(parts[p]) {
			break
		}
	}
	fixed := ""

	// this root name only works on Unix-like systems
	if name[0] == '/' {
		fixed = "/"
	}
	return fixed + path.Join(parts[:p]...), path.Join(parts[p:]...)
}

// NameFS creates an fs.FS which can be named to pass around details.
type NameFS interface {
	fs.FS
	Name() string
}
type FSWithName struct {
	fs.FS
	name string
}

var _ NameFS = FSWithName{}

func NewFSWithName(root string) NameFS {
	absRoot, err := filepath.Abs(filepath.FromSlash(root))
	name := filepath.Base(root)
	if err == nil {
		name = filepath.Base(absRoot)
	}
	return &FSWithName{
		name: name,
		FS:   os.DirFS(filepath.FromSlash(root)),
	}
}

func (f FSWithName) Name() string {
	return f.name
}

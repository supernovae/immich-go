package cachereader

import (
	"io"
	"os"
	"path/filepath"
	"sync"
	"github.com/simulot/immich-go/internal/fshelper/debugfiles"
	"github.com/simulot/immich-go/internal/fshelper/hash"
	"github.com/simulot/immich-go/internal/fshelper/osfs"
	"github.com/simulot/immich-go/internal/loghelper"
)

// CacheReader is a reader that caches the data in a temporary file to allow multiple reads
type CacheReader struct {
	mu           sync.Mutex
	tmpFile      osfs.OSFS //*os.File // tmpFile is the temporary file or the original file
	name         string
	shouldRemove bool
}

// NewCacheReader creates a new CacheReader from an io.ReadCloser
// When the reader is an os.File, it will be used directly
// Otherwise, the content will be copied into a temporary file, and the original reader will be closed
//
// The Checksum is computed on the fly
func NewCacheReader(name string, rc io.ReadCloser) (*CacheReader, string, error) {
	var err error
	var sha1Hash string

	c := &CacheReader{}
	if f, ok := rc.(osfs.OSFS); ok {
		c.name = f.Name()
		c.tmpFile = f
	} else {
		d := os.Getenv("IMMICHGO_TEMPDIR")
		if d == "" {
			d, err = os.UserCacheDir()
			if err != nil {
				d = os.TempDir()
			}
		}
		d = filepath.Join(d, "immich-go", "temp")

		err = os.MkdirAll(d, 0o700)
		if err != nil {
			d = os.TempDir()
		}
		c.tmpFile, err = os.CreateTemp(d, "immich-go_*")
		if err != nil {
			return nil, "", err
		}
		debugfiles.TrackOpenFile(c.tmpFile, c.tmpFile.Name())
		c.name = c.tmpFile.Name()

		// be sure to copy the reader content into the temporary file
		// and compute the SHA1 checksum on the fly
		sha1Hash, err = hash.Base64Encode(hash.GetSHA1Hash(io.TeeReader(rc, c.tmpFile)))
		if err != nil {
			c.tmpFile.Close()
			_ = os.Remove(c.name)
			return nil, "", err
		}
		_ = c.tmpFile.Close()
		debugfiles.TrackCloseFile(c.tmpFile)
		c.tmpFile = nil
		rc.Close()
		debugfiles.TrackCloseFile(rc)
		c.shouldRemove = true
	}
	return c, sha1Hash, err
}

// OpenFile creates a new file handler based on the temporary file
func (cr *CacheReader) OpenFile() (*tempFile, error) {
	cr.mu.Lock()
	if cr.tmpFile != nil {
		f := cr.tmpFile
		cr.tmpFile = nil
		cr.mu.Unlock()
		if osFile, ok := f.(*os.File); ok {
			if _, err := osFile.Seek(0, io.SeekStart); err == nil {
				return &tempFile{File: osFile, cr: cr}, nil
			}
		}
		debugfiles.TrackCloseFile(f)
		_ = f.Close()
	} else {
		cr.mu.Unlock()
	}
	f, err := os.Open(cr.name)
	if err != nil {
		return nil, err
	}
	debugfiles.TrackOpenFile(f, cr.name)
	return &tempFile{File: f, cr: cr}, nil
}

// Close closes the temporary file only if it was created by NewCacheReader
func (cr *CacheReader) Close() error {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	var err error
	if cr.tmpFile != nil {
		debugfiles.TrackCloseFile(cr.tmpFile)
		err = cr.tmpFile.Close()
		cr.tmpFile = nil
	}
	if err == nil && cr.shouldRemove {
		cr.shouldRemove = false
		loghelper.Debug("CacheReader: remove temporary file", "name", cr.name)
		return os.Remove(cr.name)
	}
	return err
}

type tempFile struct {
	*os.File
	cr *CacheReader
}

func (t *tempFile) Close() error {
	debugfiles.TrackCloseFile(t.File)
	err := t.File.Close()
	return err
}

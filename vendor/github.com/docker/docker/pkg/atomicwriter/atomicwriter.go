package atomicwriter

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func validateDestination(fileName string) error {
	if fileName == "" {
		return errors.New("file name is empty")
	}

	// Deliberately using Lstat here to match the behavior of [os.Rename],
	// which is used when completing the write and does not resolve symlinks.
	//
	// TODO(thaJeztah): decide whether we want to disallow symlinks or to follow them.
	if fi, err := os.Lstat(fileName); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to stat output path: %w", err)
		}
	} else if err := validateFileMode(fi.Mode()); err != nil {
		return err
	}
	if dir := filepath.Dir(fileName); dir != "" && dir != "." {
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("invalid file path: %w", err)
		}
	}
	return nil
}

func validateFileMode(mode os.FileMode) error {
	switch {
	case mode.IsRegular():
		return nil // Regular file
	case mode&os.ModeDir != 0:
		return errors.New("cannot write to a directory")
	// TODO(thaJeztah): decide whether we want to disallow symlinks or to follow them.
	// case mode&os.ModeSymlink != 0:
	// 	return errors.New("cannot write to a symbolic link directly")
	case mode&os.ModeNamedPipe != 0:
		return errors.New("cannot write to a named pipe (FIFO)")
	case mode&os.ModeSocket != 0:
		return errors.New("cannot write to a socket")
	case mode&os.ModeDevice != 0:
		if mode&os.ModeCharDevice != 0 {
			return errors.New("cannot write to a character device file")
		}
		return errors.New("cannot write to a block device file")
	case mode&os.ModeSetuid != 0:
		return errors.New("cannot write to a setuid file")
	case mode&os.ModeSetgid != 0:
		return errors.New("cannot write to a setgid file")
	case mode&os.ModeSticky != 0:
		return errors.New("cannot write to a sticky bit file")
	default:
		// Unknown file mode; let's assume it works
		return nil
	}
}

// New returns a WriteCloser so that writing to it writes to a
// temporary file and closing it atomically changes the temporary file to
// destination path. Writing and closing concurrently is not allowed.
// NOTE: umask is not considered for the file's permissions.
func New(filename string, perm os.FileMode) (io.WriteCloser, error) {
	if err := validateDestination(filename); err != nil {
		return nil, err
	}
	abspath, err := filepath.Abs(filename)
	if err != nil {
		return nil, err
	}

	f, err := os.CreateTemp(filepath.Dir(abspath), ".tmp-"+filepath.Base(filename))
	if err != nil {
		return nil, err
	}
	return &atomicFileWriter{
		f:    f,
		fn:   abspath,
		perm: perm,
	}, nil
}

// WriteFile atomically writes data to a file named by filename and with the specified permission bits.
// NOTE: umask is not considered for the file's permissions.
func WriteFile(filename string, data []byte, perm os.FileMode) error {
	f, err := New(filename, perm)
	if err != nil {
		return err
	}
	n, err := f.Write(data)
	if err == nil && n < len(data) {
		err = io.ErrShortWrite
		f.(*atomicFileWriter).writeErr = err
	}
	if err1 := f.Close(); err == nil {
		err = err1
	}
	return err
}

type atomicFileWriter struct {
	f        *os.File
	fn       string
	writeErr error
	written  bool
	perm     os.FileMode
}

func (w *atomicFileWriter) Write(dt []byte) (int, error) {
	w.written = true
	n, err := w.f.Write(dt)
	if err != nil {
		w.writeErr = err
	}
	return n, err
}

func (w *atomicFileWriter) Close() (retErr error) {
	defer func() {
		if err := os.Remove(w.f.Name()); !errors.Is(err, os.ErrNotExist) && retErr == nil {
			retErr = err
		}
	}()
	if err := w.f.Sync(); err != nil {
		_ = w.f.Close()
		return err
	}
	if err := w.f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(w.f.Name(), w.perm); err != nil {
		return err
	}
	if w.writeErr == nil && w.written {
		return os.Rename(w.f.Name(), w.fn)
	}
	return nil
}

// WriteSet is used to atomically write a set
// of files and ensure they are visible at the same time.
// Must be committed to a new directory.
type WriteSet struct {
	root string
}

// NewWriteSet creates a new atomic write set to
// atomically create a set of files. The given directory
// is used as the base directory for storing files before
// commit. If no temporary directory is given the system
// default is used.
func NewWriteSet(tmpDir string) (*WriteSet, error) {
	td, err := os.MkdirTemp(tmpDir, "write-set-")
	if err != nil {
		return nil, err
	}

	return &WriteSet{
		root: td,
	}, nil
}

// WriteFile writes a file to the set, guaranteeing the file
// has been synced.
func (ws *WriteSet) WriteFile(filename string, data []byte, perm os.FileMode) error {
	f, err := ws.FileWriter(filename, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	n, err := f.Write(data)
	if err == nil && n < len(data) {
		err = io.ErrShortWrite
	}
	if err1 := f.Close(); err == nil {
		err = err1
	}
	return err
}

type syncFileCloser struct {
	*os.File
}

func (w syncFileCloser) Close() error {
	err := w.File.Sync()
	if err1 := w.File.Close(); err == nil {
		err = err1
	}
	return err
}

// FileWriter opens a file writer inside the set. The file
// should be synced and closed before calling commit.
func (ws *WriteSet) FileWriter(name string, flag int, perm os.FileMode) (io.WriteCloser, error) {
	f, err := os.OpenFile(filepath.Join(ws.root, name), flag, perm)
	if err != nil {
		return nil, err
	}
	return syncFileCloser{f}, nil
}

// Cancel cancels the set and removes all temporary data
// created in the set.
func (ws *WriteSet) Cancel() error {
	return os.RemoveAll(ws.root)
}

// Commit moves all created files to the target directory. The
// target directory must not exist and the parent of the target
// directory must exist.
func (ws *WriteSet) Commit(target string) error {
	return os.Rename(ws.root, target)
}

// String returns the location the set is writing to.
func (ws *WriteSet) String() string {
	return ws.root
}

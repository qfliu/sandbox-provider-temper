package sshx

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// HomeDir is where relative file paths are resolved inside a sandbox.
const HomeDir = "/home/user"

// ErrNotFound and ErrIsDir describe download problems the caller should
// report to the user (404 and 400) rather than as server errors.
var (
	ErrNotFound = errors.New("file not found")
	ErrIsDir    = errors.New("path is a directory")
)

// ResolvePath makes p absolute, treating relative paths as relative to
// HomeDir. Access control is the sandbox's own: SFTP runs as `user`.
func ResolvePath(p string) string {
	if path.IsAbs(p) {
		return path.Clean(p)
	}
	return path.Join(HomeDir, p)
}

// Upload writes r to path inside the sandbox, creating parent directories,
// and returns the number of bytes written. Data is streamed, not buffered.
func Upload(client *ssh.Client, p string, r io.Reader) (int64, error) {
	c, err := sftp.NewClient(client)
	if err != nil {
		return 0, fmt.Errorf("start sftp: %w", err)
	}
	defer c.Close()

	p = ResolvePath(p)
	if err := c.MkdirAll(path.Dir(p)); err != nil {
		return 0, fmt.Errorf("create %s: %w", path.Dir(p), err)
	}
	f, err := c.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", p, err)
	}
	n, err := io.Copy(f, r)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return n, err
}

// Download streams the file at path into w, calling setSize with its length
// first (so HTTP can send Content-Length before the body).
func Download(client *ssh.Client, p string, setSize func(int64), w io.Writer) error {
	c, err := sftp.NewClient(client)
	if err != nil {
		return fmt.Errorf("start sftp: %w", err)
	}
	defer c.Close()

	p = ResolvePath(p)
	info, err := c.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		return ErrIsDir
	}
	f, err := c.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	setSize(info.Size())
	_, err = io.Copy(w, f)
	return err
}

package sshx

import (
	"crypto/rand"
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
//
// It's atomic: data goes to a hidden temporary file next to the target,
// which replaces the target in one rename only once the upload completed.
// A failed upload (client gone, over the size limit, disk full) leaves any
// existing file untouched (review finding M9).
func Upload(client *ssh.Client, p string, r io.Reader) (int64, error) {
	c, err := sftp.NewClient(client)
	if err != nil {
		return 0, fmt.Errorf("start sftp: %w", err)
	}
	defer c.Close()

	p = ResolvePath(p)
	dir := path.Dir(p)
	if err := c.MkdirAll(dir); err != nil {
		return 0, fmt.Errorf("create %s: %w", dir, err)
	}
	suffix := make([]byte, 6)
	rand.Read(suffix)
	tmp := path.Join(dir, fmt.Sprintf(".%s.upload-%x", path.Base(p), suffix))

	f, err := c.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", tmp, err)
	}
	n, err := io.Copy(f, &cutShort{r: r})
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		// posix-rename@openssh.com replaces an existing target atomically.
		if err = c.PosixRename(tmp, p); err != nil {
			err = fmt.Errorf("replace %s: %w", p, err)
		}
	}
	if err != nil {
		c.Remove(tmp) // best-effort; the original file is untouched
		return n, err
	}
	return n, nil
}

// cutShort turns io.ErrUnexpectedEOF into ErrClientGone. net/http returns
// it when a client disconnects mid-body, and sftp's ReadFrom treats it as a
// clean end, which would rename a partial file over the target. The error
// is sticky: a later Read can't turn it back into a clean io.EOF.
type cutShort struct {
	r   io.Reader
	err error
}

// ErrClientGone means an upload's client disconnected before sending the
// whole body.
var ErrClientGone = errors.New("upload ended early: the client disconnected")

func (c *cutShort) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	n, err := c.r.Read(p)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		c.err = ErrClientGone
		err = c.err
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

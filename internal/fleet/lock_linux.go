package fleet

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func Lock(home string) (func(), error) {
	if err := os.MkdirAll(Root(home), 0o700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(Root(home))
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("fleet directory must be a private directory, not a symlink")
	}
	f, err := os.OpenFile(filepath.Join(Root(home), "lock"), os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		f.Close()
		return nil, errors.New("fleet lock must be a private regular file")
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another fleet operation is in progress")
	}
	return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }, nil
}

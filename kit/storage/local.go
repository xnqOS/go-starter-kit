package storage

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
)

// Local keeps objects as files under one directory. Every access goes through
// os.Root, so neither keys nor symlinks can reach outside it.
type Local struct{ directory string }

// NewLocal creates directory (mode 0700) if needed. An empty directory is an
// error, so a missing setting cannot silently write into the working directory.
func NewLocal(directory string) (*Local, error) {
	if directory == "" {
		return nil, errors.New("storage directory is required")
	}
	directory, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	return &Local{directory: directory}, nil
}

func (s *Local) root(ctx context.Context, key string) (*os.Root, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !ValidKey(key) {
		return nil, ErrInvalidKey
	}
	return os.OpenRoot(s.directory)
}

func (s *Local) Put(ctx context.Context, key string, data []byte) error {
	r, err := s.root(ctx, key)
	if err != nil {
		return err
	}
	defer r.Close()
	f, err := createBlob(r, key)
	if errors.Is(err, os.ErrExist) {
		return ErrExists
	}
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	err = errors.Join(writeErr, f.Close(), ctx.Err())
	if err != nil {
		_ = r.Remove(key)
	}
	return err
}

func (s *Local) Open(ctx context.Context, key string) (io.ReadSeekCloser, error) {
	r, err := s.root(ctx, key)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	f, err := r.Open(key)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (s *Local) Delete(ctx context.Context, key string) error {
	r, err := s.root(ctx, key)
	if err != nil {
		return err
	}
	defer r.Close()
	err = r.Remove(key)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err == nil {
		// Drop parent folders once empty; Remove fails on a folder that
		// still has files, which is what we want.
		for dir := path.Dir(key); dir != "."; dir = path.Dir(dir) {
			if r.Remove(dir) != nil {
				break
			}
		}
	}
	return err
}

// createBlob creates a new file, making its folders first. A concurrent
// Delete may remove an empty folder in between, so that case is retried.
func createBlob(r *os.Root, key string) (*os.File, error) {
	for attempt := 0; ; attempt++ {
		if dir := path.Dir(key); dir != "." {
			if err := r.MkdirAll(dir, 0700); err != nil {
				return nil, err
			}
		}
		f, err := r.OpenFile(key, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if errors.Is(err, os.ErrNotExist) && attempt < 3 {
			continue
		}
		return f, err
	}
}

//go:build !windows

package archive

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// withTimeout runs fn and fails the test, instead of hanging it, when fn has
// not returned within limit. A read of a FIFO with no writer blocks forever, so
// every test that hands a special file to a reading path goes through this.
func withTimeout(t *testing.T, limit time.Duration, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(limit):
		t.Fatalf("did not return within %s: a read of a special file blocked", limit)
		return nil
	}
}

func TestHashFile_RefusesAFIFOInsteadOfBlocking(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(fifo, 0644); err != nil {
		t.Fatal(err)
	}
	err := withTimeout(t, 5*time.Second, func() error {
		_, err := hashFile(fifo)
		return err
	})
	if !errors.Is(err, ErrNotRegularFile) {
		t.Fatalf("hashing a FIFO: got %v, want ErrNotRegularFile", err)
	}
}

func TestHashFile_RefusesASocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "sk")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := hashFile(sock); err == nil {
		t.Fatal("hashing a socket succeeded")
	}
}

func TestHashFile_DoesNotFollowASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := hashFile(link); err == nil {
		t.Fatal("hashing a symlink followed it and hashed its target")
	}
}

func TestHashFile_HashesARegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatal(err)
	}
	hash, err := hashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if hash != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Errorf("hash of the empty file = %s", hash)
	}
}

// A source planned as a regular file and swapped for a FIFO before the archival
// reads it must make the archival fail, not hang on the read.
func TestExecute_ARegularFileSwappedForAFIFOFailsInsteadOfBlocking(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "f")
	if err := os.WriteFile(src, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	p, err := NewPlan(src, filepath.Join(dir, "archive"), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(src, 0644); err != nil {
		t.Fatal(err)
	}
	err = withTimeout(t, 5*time.Second, func() error {
		_, err := Execute(p)
		return err
	})
	if !errors.Is(err, ErrNotRegularFile) {
		t.Fatalf("archiving a swapped-in FIFO: got %v, want ErrNotRegularFile", err)
	}
}

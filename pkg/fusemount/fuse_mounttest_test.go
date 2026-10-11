//go:build linux && mounttest

package fusemount

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// fuseServer is the smallest FUSE daemon that makes a mount answer statfs: it
// serves INIT, GETATTR of the root and STATFS, and refuses everything else.
// Mounting through /dev/fuse by hand keeps the tests free of a FUSE library.
type fuseServer struct {
	fd      int
	mute    atomic.Bool
	closing atomic.Bool
	done    chan struct{}
	once    sync.Once
	started bool
}

const (
	fuseGetattr = 3
	fuseForget  = 2
	fuseStatfs  = 17
	fuseInit    = 26
	fuseBatch   = 42
	fuseIntr    = 36
)

// newFUSE mounts a served FUSE filesystem at dir. The mount is detached when the
// test ends.
func newFUSE(t *testing.T, dir string) *fuseServer {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open("/dev/fuse", unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open /dev/fuse (the container needs --privileged and the device): %v", err)
	}
	s := &fuseServer{fd: fd, done: make(chan struct{})}
	t.Cleanup(func() {
		s.kill()
		_ = Unmount(dir)
	})
	data := fmt.Sprintf("fd=%d,rootmode=40000,user_id=0,group_id=0", fd)
	if err := unix.Mount("test", dir, "fuse", unix.MS_NOSUID|unix.MS_NODEV, data); err != nil {
		t.Fatalf("mount fuse at %s: %v", dir, err)
	}
	s.started = true
	go s.serve()
	return s
}

// kill ends the daemon: its mounts fail with ENOTCONN from then on, and a call
// that was waiting for an answer returns.
func (s *fuseServer) kill() {
	s.once.Do(func() {
		// A read in flight would hold the device open, and with it the connection.
		s.closing.Store(true)
		if s.started {
			<-s.done
		}
		_ = unix.Close(s.fd)
	})
}

func (s *fuseServer) serve() {
	defer close(s.done)
	buf := make([]byte, 1<<17)
	for !s.closing.Load() {
		fds := []unix.PollFd{{Fd: int32(s.fd), Events: unix.POLLIN}}
		if n, err := unix.Poll(fds, 20); n == 0 || err != nil {
			continue
		}
		n, err := unix.Read(s.fd, buf)
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return
		}
		if n < 40 {
			continue
		}
		var (
			opcode = binary.NativeEndian.Uint32(buf[4:])
			unique = binary.NativeEndian.Uint64(buf[8:])
			body   = buf[40:n]
		)
		if s.mute.Load() && opcode != fuseInit {
			continue
		}
		var out []byte
		errno := int32(0)
		switch opcode {
		case fuseForget, fuseBatch, fuseIntr:
			continue
		case fuseInit:
			// major, minor, max_readahead, flags, max_background (16), congestion (16), max_write.
			out = make([]byte, 64)
			binary.NativeEndian.PutUint32(out[0:], 7)
			binary.NativeEndian.PutUint32(out[4:], binary.NativeEndian.Uint32(body[4:]))
			binary.NativeEndian.PutUint32(out[8:], binary.NativeEndian.Uint32(body[8:]))
			binary.NativeEndian.PutUint16(out[16:], 16)
			binary.NativeEndian.PutUint16(out[18:], 12)
			binary.NativeEndian.PutUint32(out[20:], 1<<16)
		case fuseStatfs:
			out = make([]byte, 80)
		case fuseGetattr:
			// attr_valid (64), nsec (32), dummy (32), then fuse_attr.
			out = make([]byte, 104)
			binary.NativeEndian.PutUint64(out[0:], 1)
			binary.NativeEndian.PutUint64(out[16:], 1)                     // ino
			binary.NativeEndian.PutUint32(out[16+60:], unix.S_IFDIR|0o755) // mode
			binary.NativeEndian.PutUint32(out[16+64:], 2)                  // nlink
			binary.NativeEndian.PutUint32(out[16+80:], 4096)               // blksize
		default:
			errno = -int32(unix.ENOSYS)
		}
		reply := make([]byte, 16+len(out))
		binary.NativeEndian.PutUint32(reply[0:], uint32(len(reply)))
		binary.NativeEndian.PutUint32(reply[4:], uint32(errno))
		binary.NativeEndian.PutUint64(reply[8:], unique)
		copy(reply[16:], out)
		if _, err := unix.Write(s.fd, reply); err != nil {
			return
		}
	}
}

func TestStatusOfFUSEMount(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mnt")
	srv := newFUSE(t, dir)

	if got := Status(dir); got != Live {
		t.Fatalf("Status of a served mount = %v, want live", got)
	}
	if !IsFUSE(dir) {
		t.Error("IsFUSE of a served mount = false")
	}
	if got, err := NewProber(5 * time.Second).Probe(dir); got != Live || err != nil {
		t.Errorf("Probe = %v, %v; want live", got, err)
	}

	srv.kill()
	if got := Status(dir); got != Dead {
		t.Fatalf("Status after the daemon died = %v, want dead", got)
	}
	if IsFUSE(dir) {
		t.Error("IsFUSE of a dead mount = true")
	}

	if err := Unmount(dir); err != nil {
		t.Fatal(err)
	}
	if got := Status(dir); got != Other {
		t.Errorf("Status after Unmount = %v, want other", got)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if got := Status(dir); got != Gone {
		t.Errorf("Status of the removed path = %v, want gone", got)
	}
}

// A daemon that is alive but stuck never answers statfs. The path is reported as
// hung without blocking, and is probed again once the daemon is gone.
func TestProberWithAStuckDaemon(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mnt")
	srv := newFUSE(t, dir)
	srv.mute.Store(true)

	p := NewProber(200 * time.Millisecond)
	if _, err := p.Probe(dir); !errors.Is(err, ErrHung) {
		t.Fatalf("Probe of a stuck mount = %v, want %v", err, ErrHung)
	}
	if _, err := p.Probe(dir); !errors.Is(err, ErrHung) {
		t.Fatalf("second Probe = %v, want %v", err, ErrHung)
	}

	// Killing the daemon aborts the connection, which returns the stuck statfs.
	srv.kill()
	for range 100 {
		if state, err := p.Probe(dir); !errors.Is(err, ErrHung) {
			if state != Dead {
				t.Fatalf("Probe after the daemon died = %v, %v; want dead", state, err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the path stayed hung after the daemon died")
}

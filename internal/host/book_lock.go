package host

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
)

const bookLockFile = ".ainovel.lock"

// ErrBookInUse means the same novel directory is already held by another process.
var ErrBookInUse = errors.New("Thư mục tiểu thuyết đang bị một tiến trình ainovel-cli khác chiếm giữ")

// bookLease holds cross-process exclusive ownership of the novel directory for the Host's whole lifecycle.
// The lock file stays in the directory; the actual occupancy state is managed by the operating system and is released automatically even if the process exits abnormally.
type bookLease struct {
	lock *flock.Flock
}

func acquireBookLease(dir string) (*bookLease, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("Phân giải thư mục tiểu thuyết: %w", err)
	}
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		return nil, fmt.Errorf("Tạo thư mục tiểu thuyết: %w", err)
	}
	fileLock := flock.New(filepath.Join(absDir, bookLockFile), flock.SetPermissions(0o600))
	locked, err := fileLock.TryLock()
	if err != nil {
		return nil, closeBookLockAfterFailure(fileLock, fmt.Errorf("Chiếm giữ thư mục tiểu thuyết %q: %w", absDir, err))
	}
	if !locked {
		return nil, closeBookLockAfterFailure(fileLock, fmt.Errorf(
			"%w: %s; hãy đóng terminal khác đang thao tác thư mục này, hoặc dùng thư mục tiểu thuyết khác",
			ErrBookInUse,
			absDir,
		))
	}
	return &bookLease{lock: fileLock}, nil
}

func closeBookLockAfterFailure(fileLock *flock.Flock, cause error) error {
	if err := fileLock.Close(); err != nil {
		return errors.Join(cause, fmt.Errorf("Đóng khóa thư mục tiểu thuyết: %w", err))
	}
	return cause
}

func (l *bookLease) Close() error {
	if l == nil || l.lock == nil {
		return nil
	}
	err := l.lock.Close()
	l.lock = nil
	return err
}

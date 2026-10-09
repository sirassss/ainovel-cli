package host

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
)

const bookLockFile = ".ainovel.lock"

// ErrBookInUse 表示同一小说目录已被另一个进程占用。
var ErrBookInUse = errors.New("Thư mục tiểu thuyết đang bị một tiến trình ainovel-cli khác chiếm giữ")

// bookLease 在 Host 的完整生命周期内持有小说目录的跨进程独占权。
// 锁文件会保留在目录中；真正的占用状态由操作系统管理，进程异常退出也会自动释放。
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

//go:build unix

package cli

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// waitReadable 最多等待 100ms，让调用方定期检查取消，避免信号到达时阻塞在终端读取而无法恢复回显。
func waitReadable(input *os.File) (bool, error) {
	descriptors := []unix.PollFd{{Fd: int32(input.Fd()), Events: unix.POLLIN}}
	for {
		count, err := unix.Poll(descriptors, 100)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return count > 0, err
	}
}

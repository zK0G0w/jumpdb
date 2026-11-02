package cli

import (
	"context"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

func readSecret(ctx context.Context, input, output *os.File, label string) (string, error) {
	state, err := term.MakeRaw(int(input.Fd()))
	if err != nil {
		return "", errors.New("无法关闭终端输入回显")
	}
	defer term.Restore(int(input.Fd()), state)
	terminal := term.NewTerminal(&terminalIO{ctx: ctx, input: input, output: output}, "")
	value, err := terminal.ReadPassword(label)
	if errors.Is(err, io.EOF) || ctx.Err() != nil {
		return "", context.Canceled
	}
	if err != nil {
		return "", errors.New("终端录入失败，未保存配置")
	}
	return value, nil
}

type terminalIO struct {
	ctx    context.Context
	input  *os.File
	output *os.File
}

func (terminal *terminalIO) Read(buffer []byte) (int, error) {
	descriptors := []unix.PollFd{{Fd: int32(terminal.input.Fd()), Events: unix.POLLIN}}
	for {
		if err := terminal.ctx.Err(); err != nil {
			return 0, err
		}
		// 定期检查取消，避免信号到达时阻塞在终端读取，确保恢复回显。
		count, err := unix.Poll(descriptors, 100)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if count > 0 {
			return terminal.input.Read(buffer)
		}
	}
}

func (terminal *terminalIO) Write(buffer []byte) (int, error) {
	return terminal.output.Write(buffer)
}

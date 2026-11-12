package cli

import (
	"context"
	"errors"
	"io"
	"os"

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
	for {
		if err := terminal.ctx.Err(); err != nil {
			return 0, err
		}
		ready, err := waitReadable(terminal.input)
		if err != nil {
			return 0, err
		}
		if ready {
			return terminal.input.Read(buffer)
		}
	}
}

func (terminal *terminalIO) Write(buffer []byte) (int, error) {
	return terminal.output.Write(buffer)
}

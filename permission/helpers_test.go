package permission

import (
	"context"
	"strings"
)

type fakeCommandRunner struct {
	calls      int
	gotDir     string
	gotCommand string
	out        []byte
	exit       int
	err        error
}

func (runner *fakeCommandRunner) RunCommand(_ context.Context, dir, command string) ([]byte, int, error) {
	runner.calls++
	runner.gotDir = dir
	runner.gotCommand = command
	return runner.out, runner.exit, runner.err
}

func strconvQuote(value string) string {
	var result strings.Builder
	result.WriteByte('"')
	for _, char := range value {
		switch char {
		case '"':
			result.WriteString(`\"`)
		case '\\':
			result.WriteString(`\\`)
		default:
			result.WriteRune(char)
		}
	}
	result.WriteByte('"')
	return result.String()
}

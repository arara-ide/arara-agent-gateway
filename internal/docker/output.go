package docker

import (
	"context"
	"io"
)

type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

func Output(ctx context.Context, e Engine, spec ExecSpec, stdin []byte, limit int64) (Result, error) {
	spec.Stdin = stdin != nil
	p, err := e.Exec(ctx, spec)
	if err != nil {
		return Result{}, err
	}
	defer p.Close()
	if stdin != nil {
		if _, err := p.Write(stdin); err != nil {
			return Result{}, err
		}
		_ = p.CloseStdin()
	}
	out, _ := io.ReadAll(io.LimitReader(p.Stdout(), limit))
	_, _ = io.Copy(io.Discard, p.Stdout())
	code, err := p.Wait(ctx)
	return Result{Stdout: string(out), Stderr: p.StderrTail(), ExitCode: code}, err
}

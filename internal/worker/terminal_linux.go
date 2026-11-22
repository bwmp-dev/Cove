package worker

import (
	"github.com/creack/pty"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

func startTerminal(dir string, columns, rows uint16) (*os.File, func(uint16, uint16) error, func(), error) {
	cmd := exec.Command("/bin/sh", "-i")
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "TERM=xterm-256color", "HOME=" + dir, "LANG=C.UTF-8"}
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: columns, Rows: rows})
	if err != nil {
		return nil, nil, nil, err
	}
	var once sync.Once
	stop := func() { once.Do(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGHUP); _ = f.Close() }) }
	go func() { _ = cmd.Wait(); stop() }()
	return f, func(c, r uint16) error { return pty.Setsize(f, &pty.Winsize{Cols: c, Rows: r}) }, stop, nil
}

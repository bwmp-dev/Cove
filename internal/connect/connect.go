package connect

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

func Run(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("connect", flag.ContinueOnError)
	host := flags.String("host", "", "worker SSH hostname")
	port := flags.Int("ssh-port", 22, "worker SSH port")
	local := flags.Int("local-port", 15432, "local database port")
	key := flags.String("host-key", "", "pinned worker SSH public host key")
	identity := flags.String("identity", "", "private key file; otherwise use the SSH agent")
	until := flags.String("expires", "", "access expiry in RFC3339")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *host == "" || strings.HasPrefix(*host, "-") || strings.ContainsAny(*host, "\r\n\x00 ") || *port < 1 || *port > 65535 || *local < 1 || *local > 65535 {
		return errors.New("invalid host or port")
	}
	expires, err := time.Parse(time.RFC3339, *until)
	if err != nil || time.Until(expires) <= 0 || time.Until(expires) > time.Hour {
		return errors.New("invalid or expired access deadline")
	}
	public, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(*key))
	if err != nil || len(options) > 0 || len(rest) > 0 {
		return errors.New("invalid pinned host key")
	}
	file, err := os.CreateTemp("", "cove-known-hosts-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, err = io.WriteString(file, knownhosts.Line([]string{net.JoinHostPort(*host, strconv.Itoa(*port))}, public)+"\n")
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithDeadline(ctx, expires)
	defer cancel()
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(*local)))
	if err != nil {
		return err
	}
	defer listener.Close()
	stop := context.AfterFunc(ctx, func() { listener.Close() })
	defer stop()
	fmt.Fprintf(os.Stderr, "Database access on %s until %s. Keep this process open; Ctrl+C closes access.\n", listener.Addr(), expires.Format(time.RFC3339))
	slots := make(chan struct{}, 8)
	var sessions sync.WaitGroup
	defer sessions.Wait()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			cancel()
			return err
		}
		select {
		case slots <- struct{}{}:
		default:
			conn.Close()
			continue
		}
		sessions.Go(func() {
			defer func() { conn.Close(); <-slots }()
			command := []string{"-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + file.Name(), "-o", "GlobalKnownHostsFile=" + file.Name(), "-o", "ClearAllForwardings=yes", "-o", "ConnectTimeout=10", "-p", strconv.Itoa(*port)}
			if *identity != "" {
				command = append(command, "-i", *identity, "-o", "IdentitiesOnly=yes")
			}
			command = append(command, "cove@"+*host, "database-access")
			process := exec.CommandContext(ctx, "ssh", command...)
			input, err := process.StdinPipe()
			if err != nil {
				fmt.Fprintln(os.Stderr, "Database SSH session could not start:", err)
				return
			}
			output, err := process.StdoutPipe()
			if err != nil {
				input.Close()
				fmt.Fprintln(os.Stderr, "Database SSH session could not start:", err)
				return
			}
			process.Stderr = os.Stderr
			process.WaitDelay = 2 * time.Second
			stopConnection := context.AfterFunc(ctx, func() { conn.Close() })
			defer stopConnection()
			if err := process.Start(); err != nil {
				input.Close()
				output.Close()
				fmt.Fprintln(os.Stderr, "Database SSH session could not start:", err)
				return
			}
			inputDone := make(chan struct{})
			go func() {
				io.Copy(input, conn)
				input.Close()
				close(inputDone)
			}()
			_, outputErr := io.Copy(conn, output)
			conn.Close()
			<-inputDone
			if err := errors.Join(outputErr, process.Wait()); err != nil && ctx.Err() == nil {
				fmt.Fprintln(os.Stderr, "Database SSH session closed:", err)
			}
		})
	}
}

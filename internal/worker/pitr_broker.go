package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type pitrChild struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func startPITRChild(ctx context.Context, arguments, environment []string) (*pitrChild, error) {
	ctx, cancel := context.WithCancel(ctx)
	command := pitrToolCommand(ctx, arguments, environment)
	command.Stdout = io.Discard
	if err := command.Start(); err != nil {
		cancel()
		return nil, err
	}
	child := &pitrChild{cancel: cancel, done: make(chan struct{})}
	go func() { child.err = command.Wait(); close(child.done) }()
	return child, nil
}

func (child *pitrChild) stop() {
	if child == nil {
		return
	}
	child.cancel()
	<-child.done
}

func obscurePITRStoragePassword(ctx context.Context, environment []string) ([]string, error) {
	result := []string{}
	for _, entry := range environment {
		password, found := strings.CutPrefix(entry, "COVE_REMOTE_PASSWORD=")
		if !found {
			result = append(result, entry)
			continue
		}
		command := pitrToolCommand(ctx, []string{"rclone", "obscure", "-"}, nil)
		command.Stdin = strings.NewReader(password)
		output := &limitedBuffer{Limit: 16384}
		command.Stdout = output
		if err := command.Run(); err != nil {
			return nil, errors.New("PITR storage credentials could not be prepared")
		}
		obscured := strings.TrimSpace(output.data.String())
		if obscured == "" || strings.ContainsAny(obscured, "\r\n") {
			return nil, errors.New("invalid PITR storage credential response")
		}
		result = append(result, "RCLONE_CONFIG_COVE_PASS="+obscured)
	}
	return result, nil
}

type pitrBroker struct {
	child       *pitrChild
	server      *http.Server
	done        chan struct{}
	err         error
	quota       *pitrQuota
	root        string
	environment []string
}

func (broker *pitrBroker) close() error {
	result := broker.server.Close()
	broker.child.stop()
	<-broker.done
	if err := broker.err; err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
		result = errors.Join(result, errors.New("PITR archive admission server stopped unexpectedly"))
	}
	return result
}

func startPITRBroker(ctx context.Context, config pitrRunnerConfig) (broker *pitrBroker, result error) {
	if err := validatePITRRunner(config); err != nil {
		return nil, err
	}
	root, environment, result := pitrStorageEnvironment(config)
	if result != nil {
		return nil, result
	}
	environment, result = obscurePITRStoragePassword(ctx, environment)
	if result != nil {
		return nil, result
	}
	if err := ensurePITRArchive(ctx, config, root, environment); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		return nil, err
	}
	var material [48]byte
	if _, err := rand.Read(material[:]); err != nil {
		return nil, err
	}
	access, secret := hex.EncodeToString(material[:16]), hex.EncodeToString(material[16:])
	brokerEnvironment := append(append([]string{}, environment...), "RCLONE_AUTH_KEY="+strconv.Quote(access+","+secret))
	address := "127.0.0.1:" + strconv.Itoa(port)
	arguments := []string{"rclone", "serve", "s3", root, "--addr", address, "--metadata-max-objects", "100000", "--multipart-max-uploads", "4", "--multipart-streaming-buffer-total", "16M", "--vfs-cache-mode", "off"}
	child, result := startPITRChild(ctx, arguments, brokerEnvironment)
	if result != nil {
		return nil, errors.New("PITR storage bridge could not start")
	}
	var startedBroker *pitrBroker
	defer func() {
		if result != nil {
			if startedBroker != nil {
				result = errors.Join(result, startedBroker.close())
			} else {
				child.stop()
			}
		}
	}()
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	deadline := time.Now().Add(10 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-child.done:
			return nil, errors.New("PITR storage bridge stopped during startup")
		default:
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/", nil)
		if err != nil {
			return nil, err
		}
		response, err := client.Do(request)
		if err == nil {
			response.Body.Close()
			if response.StatusCode != http.StatusForbidden {
				return nil, errors.New("PITR storage bridge did not reject anonymous access")
			}
			ready = true
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if !ready {
		return nil, errors.New("PITR storage bridge readiness timed out")
	}
	inventory, err := pitrArchiveInventory(ctx, root, config, environment)
	if err != nil {
		return nil, err
	}
	front, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	target, err := url.Parse("http://" + address)
	if err != nil {
		front.Close()
		return nil, err
	}
	quota := newPITRQuota(config.Policy.Settings.MaxBytes, inventory)
	server := &http.Server{Handler: pitrQuotaProxy(target, config.Policy.Archive, quota), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Minute, WriteTimeout: 5 * time.Minute, MaxHeaderBytes: 16 << 10}
	startedBroker = &pitrBroker{child: child, server: server, done: make(chan struct{}), quota: quota, root: root, environment: environment}
	broker = startedBroker
	go func() { startedBroker.err = server.Serve(front); close(startedBroker.done) }()
	port = front.Addr().(*net.TCPAddr).Port
	if err := os.MkdirAll("/tmp/pitr", 0700); err != nil {
		return nil, err
	}
	if err := writeJSON(pitrWALGFile, pitrWALGConfig(config, port, access, secret)); err != nil {
		return nil, err
	}
	return broker, nil
}

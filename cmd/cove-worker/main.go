package main

import (
	"context"
	"cove/internal/worker"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	expectedArgs := 2
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "certificate-probe":
			if len(os.Args) == 3 {
				expectedArgs = 3
			}
		case "tunnel", "healthcheck", "snapshot-reap":
			expectedArgs = 3
		case "host-security-run":
			expectedArgs = 5
		case "firewall-revert", "firewall-restore":
			expectedArgs = 4
		}
	}
	if len(os.Args) != expectedArgs {
		fmt.Fprintln(os.Stderr, "usage: cove-worker COMMAND [ARGS]; tunnel, healthcheck and snapshot-reap need one argument; firewall-revert and firewall-restore need two; host-security-run needs root, owner and manager; certificate-probe accepts an optional file")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "host-gpus":
		err = worker.RunHostGPUs(ctx, os.Stdin, os.Stdout)
	case "host-firewall":
		err = worker.RunHostFirewall(ctx, os.Stdin, os.Stdout)
	case "firewall-revert":
		err = worker.RunFirewallRevert(ctx, os.Args[2], os.Args[3])
	case "firewall-restore":
		err = worker.RunFirewallRestore(ctx, os.Args[2], os.Args[3])
	case "host-maintenance":
		err = worker.RunHostMaintenance(ctx, os.Stdin, os.Stdout)
	case "pitr-fetch":
		err = worker.RunPITRFetch(ctx, os.Stdin)
	case "pitr-forget":
		err = worker.RunPITRForget(ctx)
	case "pitr-base":
		err = worker.RunPITRBase(ctx)
	case "pitr-runner":
		err = worker.RunPITRRunner(ctx)
	case "snapshot-reap":
		err = worker.RunFilesystemSnapshotReaper(ctx, os.Args[2], false)
	case "snapshot-host":
		err = worker.RunFilesystemSnapshotHost(ctx, os.Stdin, os.Stdout)
	case "healthcheck":
		err = worker.HTTPHealth(ctx, os.Args[2])
	case "tunnel":
		err = worker.TunnelProxy(ctx, "/run/cove/worker.sock", os.Args[2], os.Stdin, os.Stdout)
	case "ingress-prepare":
		engine := &worker.Engine{Root: "/var/lib/cove", Run: worker.RunCommand, ACMEDirectory: os.Getenv("COVE_ACME_DIRECTORY"), ACMERoot: os.Getenv("COVE_ACME_ROOT")}
		err = engine.PrepareIngress(ctx, os.Stdout)
	case "volume-copy":
		err = worker.VolumeCopy("/from", "/to", os.Stdout)
	case "volume-free":
		err = worker.VolumeFree("/to", os.Stdout)
	case "volume-digest":
		err = worker.VolumeDigest("/from", os.Stdout)
	case "link-gateway":
		reload := make(chan os.Signal, 1)
		signal.Notify(reload, syscall.SIGHUP)
		err = worker.RunLinkGateway(ctx, reload)
	case "host-security-run":
		err = worker.RunScheduledHostSecurity(ctx, os.Args[2], os.Args[3], os.Args[4])
	case "certificate-probe":
		var file *os.File
		path := "/config/probes.json"
		if len(os.Args) == 3 {
			path = os.Args[2]
		}
		file, err = os.Open(path)
		if err == nil {
			defer file.Close()
			err = worker.RunRouteCertificateProbe(ctx, file)
		}
	case "route-helper":
		err = worker.RunRouteAuxiliary(ctx)
	case "idle-gateway":
		err = worker.RunIdleGateway(ctx)
	case "probe":
		err = worker.Serve(ctx, os.Stdout)
	case "rpc":
		err = worker.Proxy(ctx, "/run/cove/worker.sock", os.Stdin, os.Stdout)
	case "daemon":
		var engine *worker.Engine
		engine, err = worker.NewEngine("/var/lib/cove")
		if err == nil {
			engine.ACMEDirectory = os.Getenv("COVE_ACME_DIRECTORY")
			engine.ACMERoot = os.Getenv("COVE_ACME_ROOT")
			err = engine.Listen(ctx, "/run/cove/worker.sock")
		}
	default:
		err = fmt.Errorf("unknown worker command")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

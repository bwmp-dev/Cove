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
	if len(os.Args) < 2 || ((os.Args[1] == "firewall-revert" || os.Args[1] == "firewall-restore") && len(os.Args) != 4) || ((os.Args[1] == "tunnel" || os.Args[1] == "snapshot-reap") && len(os.Args) != 3) || ((os.Args[1] != "tunnel" && os.Args[1] != "snapshot-reap" && os.Args[1] != "firewall-revert" && os.Args[1] != "firewall-restore") && len(os.Args) != 2) {
		fmt.Fprintln(os.Stderr, "usage: cove-worker probe|rpc|daemon|ingress-prepare|link-gateway|volume-copy|volume-free|volume-digest")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
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

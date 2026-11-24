package main

import (
	"context"
	"cove/internal/worker"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

func main() {
	if filepath.Base(os.Args[0]) == "mise-"+worker.RailpackMiseVersion {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := worker.RunRailpackMise(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) != 2 && !((len(os.Args) == 3 || len(os.Args) == 4) && os.Args[1] == "cloudflare-probe") && !(len(os.Args) == 3 && (os.Args[1] == "tunnel" || os.Args[1] == "healthcheck" || os.Args[1] == "certificate-probe" || os.Args[1] == "build-recipe-output" || os.Args[1] == "buildpacks-helper")) {
		fmt.Fprintln(os.Stderr, "usage: cove-worker probe|rpc|daemon|ingress-prepare|link-gateway|idle-gateway|volume-copy|volume-free|volume-digest")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "buildpacks-helper":
		if len(os.Args) != 3 {
			err = fmt.Errorf("buildpacks helper action required")
		} else {
			err = worker.RunBuildpacksHelper(ctx, os.Args[2], os.Stdout)
		}
	case "build-recipe-output":
		if len(os.Args) != 3 {
			err = fmt.Errorf("planner output kind required")
		} else {
			err = worker.ReadRailpackOutput(os.Args[2], os.Stdout)
		}
	case "build-recipe-plan":
		err = worker.RunRailpackPlanner(ctx)
	case "build-recipe-egress":
		err = worker.RunBuildRecipeEgress(ctx)
	case "static-site":
		err = worker.ServeStaticSite(ctx)
	case "build-cache-filesystem":
		err = worker.BuildCacheFilesystem(os.Stdout)
	case "registry-storage":
		var file *os.File
		file, err = os.Open("/config/registry.json")
		if err == nil {
			defer file.Close()
			err = worker.RunManagedRegistryStorage("/registry", file, os.Stdout)
		}
	case "cloudflare-connector-origin-probe":
		err = worker.RunCloudflareConnectorOriginProbe(ctx)
	case "cloudflare-origin-probe":
		err = worker.RunCloudflareOriginProbe(ctx)
	case "cloudflare-origin":
		err = worker.RunCloudflareOrigin(ctx)
	case "cloudflare-probe":
		revision := ""
		if len(os.Args) == 4 {
			revision = os.Args[3]
		}
		err = worker.RunCloudflareProbe(ctx, os.Args[2], revision)
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

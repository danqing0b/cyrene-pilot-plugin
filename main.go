// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/danqing0b/cyrene-pilot-plugin/internal/automation"
	"github.com/danqing0b/cyrene-pilot-plugin/internal/wire"
	"google.golang.org/grpc"
)

func localListener(address string) (net.Listener, string, error) {
	if path, ok := strings.CutPrefix(address, "unix:"); ok {
		if !filepath.IsAbs(path) {
			return nil, "", errors.New("Unix listen path must be absolute")
		}
		l, err := net.Listen("unix", path)
		if err != nil {
			return nil, "", err
		}
		if err := os.Chmod(path, 0600); err != nil {
			l.Close()
			return nil, "", err
		}
		return l, "grpc://unix:" + path, nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil || !isLoopback(host) {
		return nil, "", errors.New("local gRPC requires a loopback address or Unix socket")
	}
	l, err := net.Listen("tcp", address)
	if err != nil {
		return nil, "", err
	}
	return l, "grpc://" + l.Addr().String(), nil
}

func start(ctx context.Context, args []string) error {
	parser := flag.NewFlagSet("cyrene-release-pilot", flag.ContinueOnError)
	wantCapability := parser.String("capability", "", "Platform capability")
	wantVersion := parser.String("interface-version", "", "Capability version")
	listen := parser.String("listen", "", "Platform gRPC listener")
	defaultConfig := os.Getenv("CYRENE_PILOT_CONFIG_FILE")
	if defaultConfig == "" {
		defaultConfig = os.Getenv("CYRENE_RELEASE_PILOT_CONFIG_FILE")
	}
	configPath := parser.String("config", defaultConfig, "Host-owned plugin configuration")
	if err := parser.Parse(args); err != nil {
		return err
	}
	binding, storage := os.Getenv("CYRENE_CAPABILITY_BINDING_ID"), os.Getenv("CYRENE_PLUGIN_STORAGE_PATH")
	if *wantCapability != capability || *wantVersion != version || parser.NArg() != 0 ||
		!identifier.MatchString(binding) || !filepath.IsAbs(storage) || !filepath.IsAbs(*configPath) {
		return errors.New("Platform capability, interface, binding, private storage and configuration are required")
	}
	info, err := os.Stat(storage)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("Platform storage must be a private directory")
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	var flow *automation.Service
	if cfg.Automation != nil {
		flow, err = automation.Open(storage, *cfg.Automation)
		if err != nil {
			return err
		}
		defer flow.Close()
	}
	var work *coordinator
	if len(cfg.Candidates) > 0 {
		state, err := openStore(storage, cfg.Baseline)
		if err != nil {
			return err
		}
		defer state.db.Close()
		work = &coordinator{store: state, product: newProducts(cfg), now: time.Now, wake: make(chan struct{}, 1)}
	}

	direct, ref, err := localListener(*listen)
	if err != nil {
		return err
	}
	defer direct.Close()
	web, err := net.Listen("tcp", cfg.HTTPListen)
	if err != nil {
		return err
	}
	defer web.Close()
	server := grpc.NewServer(grpc.MaxRecvMsgSize(maxBody+1024), grpc.MaxSendMsgSize(maxBody+1024))
	wire.RegisterDirectPluginRuntimeServer(server, &runtime{binding: binding, coordinator: work, automation: flow})
	var legacy http.Handler = http.NotFoundHandler()
	if work != nil {
		legacy = work.routes()
	}
	var flows http.Handler
	if flow != nil {
		flows = flow.Handler()
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/healthz" {
			writeJSON(w, 200, map[string]string{"status": "ok"})
			return
		}
		if flows != nil && (strings.HasPrefix(r.URL.Path, "/admin/jobs/") || strings.HasPrefix(r.URL.Path, "/admin/tools/")) {
			if !authorized(r, cfg.adminToken) {
				writeJSON(w, 401, map[string]string{"error": "operator authorization required"})
				return
			}
			flows.ServeHTTP(w, r)
			return
		}
		legacy.ServeHTTP(w, r)
	})
	httpServer := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 180 * time.Second, IdleTimeout: 30 * time.Second}
	failures := make(chan error, 2)
	go func() { failures <- server.Serve(direct) }()
	go func() { failures <- httpServer.Serve(web) }()
	defer server.Stop()
	defer httpServer.Close()

	// The first stdout line is reserved for the Package Runtime supervisor.
	if err := json.NewEncoder(os.Stdout).Encode(map[string]string{
		"event": "direct_plugin_ready", "connection_ref": ref,
		"capability": capability, "interface_version": version,
	}); err != nil {
		return err
	}
	log.Printf("Cyrene Pilot operator endpoint at %s", web.Addr())
	runContext, cancel := context.WithCancel(ctx)
	var done sync.WaitGroup
	if work != nil {
		done.Go(func() { work.run(runContext) })
	}
	if flow != nil {
		done.Go(func() { flow.Run(runContext) })
	}
	defer func() { cancel(); done.Wait() }()
	select {
	case <-ctx.Done():
		return nil
	case err := <-failures:
		return err
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := start(ctx, os.Args[1:]); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

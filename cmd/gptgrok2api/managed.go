package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/config"
	"github.com/auucoder/gptgrok2api-go/internal/httpapi"
	"github.com/auucoder/gptgrok2api-go/internal/rolling"
)

func managedMain() {
	controlAddr := os.Getenv("CFM_ROLLING_CONTROL_ADDR")
	host, _, err := net.SplitHostPort(controlAddr)
	if err != nil || host != "127.0.0.1" {
		log.Fatal("rolling control must bind container loopback")
	}
	statePath := os.Getenv("CFM_ROLLING_STATE_FILE")
	leasePath := os.Getenv("CFM_WRITER_LEASE_FILE")
	instance, err := rolling.NewInstance(leasePath, statePath, func() (*rolling.Application, error) {
		// Config loading can migrate/write files; even this stays behind the lease.
		cfg, err := config.Load("")
		if err != nil {
			return nil, err
		}
		if file, err := os.OpenFile(filepath.Join(cfg.RootDir, "logs", "app.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600); err == nil {
			log.SetOutput(io.MultiWriter(os.Stdout, file))
		}
		app := httpapi.NewWithWorkers(cfg, false)
		if err := app.DeploymentReady(); err != nil {
			return nil, err
		}
		return &rolling.Application{Handler: app.Handler(), Pause: app.PauseDeployment, Status: app.DeploymentStatus, Start: app.StartWorkers, Shutdown: app.ShutdownRegistration, Seal: app.SealDeployment}, nil
	})
	if err != nil {
		log.Fatal(err)
	}
	gatewayPath := os.Getenv("CFM_GATEWAY_STATE_FILE")
	if gatewayPath != "" {
		instance.SetGatewayFactory(func(existing bool) (*rolling.Gateway, error) {
			if existing {
				if _, err := os.Stat(gatewayPath); err != nil {
					return nil, err
				}
			}
			slots := []string{"http://cfm-blue:8080", "http://cfm-green:8080"}
			if value := os.Getenv("CFM_GATEWAY_SLOT_URLS"); value != "" {
				slots = strings.Split(value, ",")
			}
			if len(slots) != 2 {
				return nil, errors.New("exactly two explicit gateway slot URLs required")
			}
			return rolling.NewGateway(gatewayPath, slots[0], slots, 512, 60*time.Second, true)
		})
	}
	addr := os.Getenv("GO_LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	controlListener, err := net.Listen("tcp", controlAddr)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Handler: instance, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 300 * time.Second, IdleTimeout: 120 * time.Second}
	control := &http.Server{Handler: instance.Control(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	for _, item := range []struct {
		server   *http.Server
		listener net.Listener
	}{{server, listener}, {control, controlListener}} {
		go func(s *http.Server, l net.Listener) {
			if err := s.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatal("rolling listener failed")
			}
		}(item.server, item.listener)
	}
	_, stateErr := os.Stat(statePath)
	if err := instance.Restore(); err != nil {
		log.Print("rolling state needs operator recovery")
	} else if os.IsNotExist(stateErr) && os.Getenv("CFM_ROLLING_BOOTSTRAP_ACTIVE") == "true" {
		if err := instance.Activate(); err != nil {
			log.Fatal("legacy protocol bootstrap failed")
		}
		if err := instance.Pause(false); err != nil {
			log.Fatal(err)
		}
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
	_ = instance.Shutdown(ctx)
	_ = control.Shutdown(ctx)
	// The lease is intentionally held until process exit, including all
	// background goroutines. No successor may initialize from a stale snapshot.
}

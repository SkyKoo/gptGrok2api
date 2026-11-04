package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/rolling"
)

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
func main() {
	controlAddr := env("CFM_GATEWAY_CONTROL_ADDR", "127.0.0.1:9091")
	host, _, err := net.SplitHostPort(controlAddr)
	if err != nil || host != "127.0.0.1" {
		log.Fatal("gateway control must bind container loopback")
	}
	limit, err := strconv.Atoi(env("CFM_GATEWAY_MAX_QUEUED", "512"))
	if err != nil || limit < 1 || limit > 4096 {
		log.Fatal("invalid gateway queue limit")
	}
	seconds, err := strconv.Atoi(env("CFM_GATEWAY_WAIT_SECONDS", "60"))
	if err != nil || seconds < 1 || seconds > 120 {
		log.Fatal("invalid gateway waiting budget")
	}
	allowed := strings.Split(env("CFM_GATEWAY_SLOTS", "http://cfm-blue:8080,http://cfm-green:8080"), ",")
	gateway, err := rolling.NewGateway(env("CFM_GATEWAY_STATE_FILE", "/app/runtime/gateway.json"), env("CFM_GATEWAY_INITIAL_UPSTREAM", allowed[0]), allowed, limit, time.Duration(seconds)*time.Second)
	if err != nil {
		log.Fatal("gateway state invalid")
	}
	public := &http.Server{Addr: env("GO_LISTEN_ADDR", ":8080"), Handler: gateway, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 300 * time.Second, IdleTimeout: 120 * time.Second}
	control := &http.Server{Addr: controlAddr, Handler: gateway.Control(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	go func() {
		if err := public.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("gateway public listener failed")
		}
	}()
	go func() {
		if err := control.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("gateway control listener failed")
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	_ = public.Shutdown(ctx)
	_ = control.Shutdown(ctx)
}

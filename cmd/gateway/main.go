package main

import (
	"context"
	"errors"
	assets "feishu-gateway"
	"feishu-gateway/internal/feishu"
	"feishu-gateway/internal/gateway"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	cfg, err := gateway.LoadConfig()
	if err != nil {
		log.Fatal(err)
	}
	store, err := gateway.OpenStore(cfg.DatabasePath)
	if err != nil {
		log.Fatal("Cannot open database: ", err)
	}
	defer store.Close()
	service := gateway.NewServer(cfg, store, feishu.NewClient(), assets.Web())
	server := service.HTTPServer()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	log.Printf("Feishu Gateway listening on %s; admin auth=%s; public origin=%s", server.Addr, cfg.Mode, cfg.PublicOrigin)
	select {
	case err = <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 105*time.Second)
		defer cancel()
		if err = server.Shutdown(shutdown); err != nil {
			log.Printf("Graceful shutdown failed: %v", err)
			server.Close()
		}
		<-errCh
	}
}

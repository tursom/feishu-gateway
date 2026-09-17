package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	assets "feishu-gateway"
	"feishu-gateway/internal/app"
	"feishu-gateway/internal/feishu"
)

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
func main() {
	store, err := app.Open(env("DATABASE_PATH", "./data/gateway.sqlite"))
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	server := &http.Server{Addr: env("LISTEN_ADDR", ":8787"), Handler: app.New(store, feishu.New(), assets.Web()), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	log.Printf("Feishu Gateway listening on %s", server.Addr)
	select {
	case err = <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer cancel()
		if err = server.Shutdown(shutdown); err != nil {
			_ = server.Close()
		}
		<-done
	}
}

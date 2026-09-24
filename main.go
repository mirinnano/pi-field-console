package main

import (
	"context"
	"embed"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

//go:embed static/*
var webAssets embed.FS

func main() {
	address, err := listenAddress()
	if err != nil {
		log.Fatal(err)
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		log.Fatal(err)
	}

	bridgePath := defaultBridgePath()
	cfg := envConfig()
	app := newService(bridgePath)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go app.pollBridge(ctx, 3*time.Second)

	server := &http.Server{
		Handler:           app.handler(cfg, webAssets),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    16 * 1024,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()

	log.Printf("Pi remote console listening at http://%s (bridge socket configured)", listener.Addr())
	log.Printf("bind is loopback-only; remote access requires an authenticated local HTTPS/VPN proxy")
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// Release builds inject the Git tag with -ldflags "-X=main.version=...".
var version = "0.1.0"

func main() {
	bind := flag.String("bind", "127.0.0.1", "监听地址；局域网共享可设为 0.0.0.0")
	port := flag.Int("port", 8767, "HTTP 服务端口")
	deviceEnabled := flag.Bool("device", true, "启用内置设备协议，为 CCTV10 等频道提供高码率源")
	dataDirFlag := flag.String("data-dir", "", "内置设备身份状态目录；默认为本程序所在目录下的 data 目录")
	maxBandwidth := flag.Int64("max-bandwidth", defaultMaxBandwidth, "多码率直播选择上限（bit/s）；0 选择最高码率")
	showVersion := flag.Bool("version", false, "显示版本")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	if flag.NArg() > 0 {
		legacyPort, err := strconv.Atoi(flag.Arg(0))
		if err != nil {
			log.Fatalf("invalid positional port %q", flag.Arg(0))
		}
		*port = legacyPort
	}
	if *port < 1 || *port > 65535 {
		log.Fatal("port must be between 1 and 65535")
	}
	if *maxBandwidth < 0 {
		log.Fatal("max-bandwidth must be non-negative")
	}
	if net.ParseIP(*bind) == nil && *bind != "localhost" {
		log.Fatalf("invalid bind address %q", *bind)
	}

	executable, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	executableDir := filepath.Dir(executable)
	dataDir := filepath.Join(executableDir, "data")
	if *dataDirFlag != "" {
		dataDir, err = filepath.Abs(*dataDirFlag)
		if err != nil {
			log.Fatal(err)
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 100
	transport.MaxIdleConnsPerHost = 20
	transport.ResponseHeaderTimeout = 20 * time.Second
	transport.TLSHandshakeTimeout = 10 * time.Second
	client := &http.Client{Transport: transport, Timeout: 35 * time.Second}
	defer transport.CloseIdleConnections()
	channels := newChannelManager(client)
	channels.maxBandwidth = *maxBandwidth
	defer channels.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var device deviceSource
	if *deviceEnabled {
		device = newDeviceManager(client, filepath.Join(dataDir, "device-state-go.json"))
		device.Start(ctx)
	}
	app := newAppServer(channels, *bind, *port)
	app.ctx = ctx
	app.device = device
	httpServer := &http.Server{
		Addr:              net.JoinHostPort(*bind, strconv.Itoa(*port)),
		Handler:           app,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      90 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		channels.Close()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
		if device != nil {
			device.Close()
		}
	}()

	log.Printf("ysp-live-go %s: %d channels, listening on http://%s/", version, len(channelSpecs), httpServer.Addr)
	if *bind == "0.0.0.0" || *bind == "::" {
		log.Printf("warning: service is reachable from other hosts and has no authentication")
	}
	serveErr := httpServer.ListenAndServe()
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		log.Printf("HTTP server failed: %v", serveErr)
		stop()
	}
	<-shutdownDone
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		os.Exit(1)
	}
}

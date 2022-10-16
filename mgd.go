package main

import (
	"context"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/morgangallant/mgd/pkg/inet"
	"github.com/morgangallant/mgd/pkg/management"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

func main() {
	rand.Seed(time.Now().UnixNano())

	var logger *zap.Logger
	if local() {
		logger, _ = zap.NewDevelopment()
	} else {
		logger, _ = zap.NewProduction()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigc := make(chan os.Signal)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigc
		cancel()
	}()

	if err := run(ctx, logger); err != nil && !errors.Is(err, context.Canceled) {
		logger.Fatal("top-level error", zap.Error(err))
	}
}

type shutdownFunc func(ctx context.Context) error

func run(ctx context.Context, logger *zap.Logger) error {
	var shutdowns []shutdownFunc
	shutdowns = append(shutdowns, func(_ context.Context) error {
		return logger.Sync()
	})

	// Connect to the internal network.
	ips := inet.Wait(ctx)
	if len(ips) == 0 {
		return errors.New("failed to connect to inet")
	} else {
		logger.Info("connected to inet", zap.Stringers("addresses", ips))
	}
	shutdowns = append(shutdowns, func(_ context.Context) error {
		return inet.Shutdown()
	})

	// A channel to collect any errors from running servers.
	errc := make(chan error)

	// Spin up the management server.
	lis, err := inet.PortListener(80)
	if err != nil {
		return errors.Wrap(err, "failed to listen on port 80")
	}
	msrv := &http.Server{Handler: management.Handler()}
	go func() {
		errc <- msrv.Serve(lis)
	}()
	shutdowns = append(shutdowns, func(ctx context.Context) error {
		if err := msrv.Shutdown(ctx); err != nil {
			return errors.Wrap(err, "failed to shutdown management server")
		}
		return lis.Close()
	})

	var retErr error
	select {
	case <-ctx.Done():
		retErr = ctx.Err()
	case err := <-errc:
		retErr = err
	}

	for i := len(shutdowns) - 1; i >= 0; i-- {
		if err := shutdowns[i](context.TODO()); err != nil {
			logger.Warn("shutdown error", zap.Error(err))
		}
	}

	return retErr
}

func local() bool {
	host, _ := os.Hostname()
	return runtime.GOOS == "darwin" && host == "mg"
}

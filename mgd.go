package main

import (
	"context"
	"errors"
	"math/rand"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/morgangallant/mgd/pkg/inet"
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

	ips := inet.Wait(ctx)
	if len(ips) == 0 {
		return errors.New("failed to connect to inet")
	} else {
		logger.Info("connected to inet", zap.Stringers("addresses", ips))
	}
	shutdowns = append(shutdowns, func(_ context.Context) error {
		return inet.Shutdown()
	})

	var retErr error
	<-ctx.Done() // Wait for the shutdown signal.
	retErr = ctx.Err()

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

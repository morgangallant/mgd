// Package inet is a utility package which connects to the
// Tailscale-based internal network automatically.
package inet

import (
	"context"
	"net/http"
	"net/netip"
	"os"
	"time"

	"github.com/pkg/errors"
	"tailscale.com/tsnet"
)

var server = &tsnet.Server{
	AuthKey:   os.Getenv("TSKEY"),
	Logf:      noopLogger,
	Ephemeral: true,
}

// Status returns the status of the internal network connection.
func Status(ctx context.Context) (string, []netip.Addr, error) {
	client, err := server.LocalClient()
	if err != nil {
		return "", nil, errors.Wrap(err, "getting localclient")
	}

	status, err := client.Status(ctx)
	if err != nil {
		return "", nil, errors.Wrap(err, "getting status")
	}

	return status.BackendState, status.TailscaleIPs, nil
}

// Wait waits for the internal network connection to come alive
// by polling Status. It returns the set of addresses allocated
// to this machine by Tailscale. If a connection was unable to
// be established, a nil slice is returned.
func Wait(ctx context.Context) []netip.Addr {
	if _, ok := os.LookupEnv("TSKEY"); !ok {
		return nil
	}

	for {
		status, ips, err := Status(ctx)
		if err != nil {
			return nil
		} else if status == "Running" && len(ips) > 0 {
			return ips
		}
		time.Sleep(time.Second)
	}
}

var serverClient = &http.Client{
	Transport: &http.Transport{
		DialContext: server.Dial, // Use the Tailscale dialer.
	},
}

// HTTPClient returns an http.Client object which should be used
// for all outgoing connections. If the server isn't running, then
// http.DefaultClient is returned.
func HTTPClient() *http.Client {
	if _, ok := os.LookupEnv("TSKEY"); !ok {
		return http.DefaultClient
	}
	return serverClient
}

// Shutdown shuts down the connection to the internal network.
func Shutdown() error {
	return server.Close()
}

func noopLogger(format string, args ...any) {}

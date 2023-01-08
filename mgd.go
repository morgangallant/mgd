package main

import (
	"context"
	"log"
	"math/rand"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/joho/godotenv"
	"github.com/pkg/errors"
	"golang.org/x/exp/slices"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
)

func init() {
	rand.Seed(time.Now().UnixNano())
	_ = godotenv.Load()

	log.SetFlags(log.LstdFlags | log.Lshortfile)
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)
		<-c
		cancel()
	}()
	if err := run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("top-level error: %+v", err)
	}
}

func run(ctx context.Context) error {
	conn, err := newInetConn(ctx, "mgd")
	if err != nil {
		return err
	}
	defer conn.shutdown()
	conn.setPeerAddedCb(func(peer *tailcfg.Node) {
		log.Printf("Discovered peer %s (%v)", peer.StableID, peer.Addresses)
	})
	conn.setPeerRemovedCb(func(peer *tailcfg.Node) {
		log.Printf("Lost peer %s", peer.StableID)
	})
	log.Printf("connected to inet, self ips: %v", conn.selfIps)

	<-ctx.Done()
	return nil
}

type inetConn struct {
	inner    *tsnet.Server
	selfIps  []netip.Addr
	selfTags []string

	mu            sync.Mutex // Protects below field(s).
	peers         map[string]*tailcfg.Node
	peerAddedCb   func(peer *tailcfg.Node)
	peerRemovedCb func(peer *tailcfg.Node)
}

func newInetConn(ctx context.Context, hostname string) (*inetConn, error) {
	authKey, ok := os.LookupEnv("TSKEY")
	if !ok {
		return nil, errors.New("missing TSKEY environment variable")
	}

	dir := filepath.Join(os.TempDir(), strconv.FormatInt(time.Now().UnixNano(), 10))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, errors.WithStack(err)
	}

	ic := &inetConn{
		peers: make(map[string]*tailcfg.Node),
	}
	ic.mu.Lock()
	defer ic.mu.Unlock()

	nf := func(n *ipn.Notify) (keepGoing bool) {
		if n.NetMap == nil {
			return true
		}

		ic.mu.Lock()
		defer ic.mu.Unlock()

		existing := ic.peers
		ic.peers = make(map[string]*tailcfg.Node, len(n.NetMap.Peers))
		for _, p := range n.NetMap.Peers {
			if p.Online == nil || !*p.Online {
				continue
			}
			cloned := p.Clone()
			slices.Sort(cloned.Tags)
			if !slices.Equal(cloned.Tags, ic.selfTags) {
				continue
			}
			ic.peers[cloned.ID.String()] = cloned
			if _, ok := existing[cloned.ID.String()]; ok {
				delete(existing, cloned.ID.String())
			} else if ic.peerAddedCb != nil {
				ic.peerAddedCb(cloned)
			}
		}

		if ic.peerRemovedCb != nil {
			for _, p := range existing {
				ic.peerRemovedCb(p)
			}
		}

		return true
	}

	inner := &tsnet.Server{
		Hostname:   hostname,
		Dir:        dir,
		AuthKey:    authKey,
		Ephemeral:  true,
		Logf:       func(format string, args ...any) {},
		NotifyFunc: nf,
	}

	client, err := inner.LocalClient()
	if err != nil {
		return nil, errors.WithStack(err)
	}

	var status *ipnstate.Status
	for status == nil || status.BackendState != "Running" {
		time.Sleep(time.Millisecond * 500)
		status, err = client.Status(ctx)
		if err != nil {
			return nil, errors.WithStack(err)
		}
	}

	tags := status.Self.Tags.AsSlice()
	slices.Sort(tags)

	ic.inner = inner
	ic.selfIps = status.TailscaleIPs
	ic.selfTags = tags

	return ic, nil
}

func (c *inetConn) shutdown() error {
	return c.inner.Close()
}

func (c *inetConn) setPeerAddedCb(cb func(peer *tailcfg.Node)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.peerAddedCb = cb
}

func (c *inetConn) setPeerRemovedCb(cb func(peer *tailcfg.Node)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.peerRemovedCb = cb
}

package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log"
	"math/rand"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
	"github.com/joho/godotenv"
	"github.com/pkg/errors"
	"golang.org/x/exp/slices"
	"golang.org/x/sync/errgroup"
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
	// This is used for shutting down the server when a user presses Ctrl-C.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)
		<-c
		cancel()
	}()
	if err := run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("%+v", err)
	}
}

func run(ctx context.Context) error {
	datadir := filepath.Join(os.TempDir(), strconv.FormatInt(time.Now().UnixNano(), 10))
	if err := os.MkdirAll(datadir, 0755); err != nil {
		return errors.WithStack(err)
	}
	defer os.RemoveAll(datadir)

	conn, err := newInetConn(ctx, "mgd", filepath.Join(datadir, "ts"))
	if err != nil {
		return err
	}
	defer conn.shutdown()
	log.Printf("connected to inet as %s, self ips: %v", conn.selfId, conn.selfIps)

	manager, err := newClusterManager(conn, filepath.Join(datadir, "raft"))
	if err != nil {
		return err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.close(ctx); err != nil {
			log.Printf("error shutting down raft: %v", err)
		}
	}()
	log.Printf("started cluster manager")

	<-ctx.Done()
	return nil
}

// This structure describes a connection to the Tailscale internal network.
type inetConn struct {
	inner    *tsnet.Server
	selfId   tailcfg.StableNodeID
	selfIps  []netip.Addr
	selfTags []string

	mu            sync.Mutex // Protects below field(s).
	peers         map[tailcfg.StableNodeID]*tailcfg.Node
	peerAddedCb   func(peer *tailcfg.Node)
	peerRemovedCb func(peer *tailcfg.Node)
}

func ensureDir(dir string) error {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return errors.WithStack(err)
		}
	}
	return nil
}

func newInetConn(ctx context.Context, hostname, datadir string) (*inetConn, error) {
	authKey, ok := os.LookupEnv("TSKEY")
	if !ok {
		return nil, errors.New("missing TSKEY environment variable")
	}

	ic := &inetConn{
		peers: make(map[tailcfg.StableNodeID]*tailcfg.Node),
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
		ic.peers = make(map[tailcfg.StableNodeID]*tailcfg.Node, len(n.NetMap.Peers))
		for _, p := range n.NetMap.Peers {
			if p.Online == nil || !*p.Online {
				continue
			}
			cloned := p.Clone()
			slices.Sort(cloned.Tags)
			if !slices.Equal(cloned.Tags, ic.selfTags) {
				continue
			}
			ic.peers[cloned.StableID] = cloned
			if _, ok := existing[cloned.StableID]; ok {
				delete(existing, cloned.StableID)
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

	if err := ensureDir(datadir); err != nil {
		return nil, err
	}

	inner := &tsnet.Server{
		Hostname:   hostname,
		Dir:        datadir,
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

	ic.inner = inner
	ic.selfIps = status.TailscaleIPs
	ic.selfId = status.Self.ID

	tags := status.Self.Tags.AsSlice()
	slices.Sort(tags)
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

func (c *inetConn) listen(network, addr string) (net.Listener, error) {
	return c.inner.Listen(network, addr)
}

func (c *inetConn) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return c.inner.Dial(ctx, network, address)
}

type clusterFsm struct{}

var _ raft.FSM = (*clusterFsm)(nil)

func (fsm *clusterFsm) Apply(l *raft.Log) interface{} {
	return nil
}

func (fsm *clusterFsm) Snapshot() (raft.FSMSnapshot, error) {
	return &clusterFsmSnapshot{}, nil
}

func (fsm *clusterFsm) Restore(_ io.ReadCloser) error {
	return nil
}

type clusterFsmSnapshot struct{}

var _ raft.FSMSnapshot = (*clusterFsmSnapshot)(nil)

func (s *clusterFsmSnapshot) Persist(_ raft.SnapshotSink) error {
	return nil
}

func (s *clusterFsmSnapshot) Release() {}

type clusterStore struct {
	inner store
}

func (cs *clusterStore) Set(key, val []byte) error {
	return cs.inner.put(context.Background(), key, val)
}

func (cs *clusterStore) Get(key []byte) ([]byte, error) {
	val, err := cs.inner.get(context.Background(), key)
	if errors.Is(err, errNotFound) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return val, nil
}

func (cs *clusterStore) SetUint64(key []byte, val uint64) error {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], val)
	return cs.inner.put(context.Background(), key, buf[:])
}

func (cs *clusterStore) GetUint64(key []byte) (uint64, error) {
	val, err := cs.inner.get(context.Background(), key)
	if err != nil {
		if errors.Is(err, errNotFound) {
			return 0, nil
		}
		return 0, err
	}
	return binary.BigEndian.Uint64(val), nil
}

const logPrefix = "log:"

func keyForLog(idx uint64) []byte {
	var buf [len(logPrefix) + 8]byte
	copy(buf[:], logPrefix)
	binary.BigEndian.PutUint64(buf[len(logPrefix):], idx)
	return buf[:]
}

func (cs *clusterStore) FirstIndex() (uint64, error) {
	start, end := prefixedRange([]byte(logPrefix))
	iter, err := cs.inner.newIter(context.Background(), start, end)
	if err != nil {
		return 0, err
	}
	defer iter.close()

	if !iter.first() {
		return 0, nil
	}

	return binary.BigEndian.Uint64(iter.key()[len(logPrefix):]), nil
}

func (cs *clusterStore) LastIndex() (uint64, error) {
	start, end := prefixedRange([]byte(logPrefix))
	iter, err := cs.inner.newIter(context.Background(), start, end)
	if err != nil {
		return 0, err
	}
	defer iter.close()

	if !iter.last() {
		return 0, nil
	}
	return binary.BigEndian.Uint64(iter.key()[len(logPrefix):]), nil
}

func (cs *clusterStore) GetLog(idx uint64, log *raft.Log) error {
	val, err := cs.inner.get(context.Background(), keyForLog(idx))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(val, log); err != nil {
		return errors.WithStack(err)
	}
	return nil
}

func (cs *clusterStore) StoreLog(log *raft.Log) error {
	buf, err := json.Marshal(log)
	if err != nil {
		return errors.WithStack(err)
	}
	return cs.inner.put(context.Background(), keyForLog(log.Index), buf)
}

func (cs *clusterStore) StoreLogs(logs []*raft.Log) error {
	for _, log := range logs {
		if err := cs.StoreLog(log); err != nil {
			return err
		}
	}
	return nil
}

func (cs *clusterStore) DeleteRange(min, max uint64) error {
	if err := deleteRange(cs.inner, keyForLog(min), keyForLog(max)); err != nil {
		return err
	}
	return nil
}

var (
	_ raft.LogStore    = (*clusterStore)(nil)
	_ raft.StableStore = (*clusterStore)(nil)
)

const clusterPort = "11106"

type clusterManager struct {
	selfId        tailcfg.StableNodeID
	inner         *raft.Raft
	fsm           *clusterFsm
	errGroup      *errgroup.Group
	shutdownFuncs []func(context.Context) error
}

func newClusterManager(
	conn *inetConn,
	datadir string,
) (*clusterManager, error) {
	manager := &clusterManager{
		selfId:   conn.selfId,
		fsm:      &clusterFsm{},
		errGroup: new(errgroup.Group),
	}

	pebble, err := newPebbleStore(filepath.Join(datadir, "store"), true)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	manager.shutdownFuncs = append(manager.shutdownFuncs, func(_ context.Context) error {
		return pebble.close()
	})
	store := &clusterStore{inner: pebble}

	snapshots, err := raft.NewFileSnapshotStore(datadir, 2, os.Stderr)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if len(conn.selfIps) == 0 {
		return nil, errors.New("no self ip")
	}
	serverAddr := net.JoinHostPort(conn.selfIps[0].String(), clusterPort)

	streamLayer, err := newInetStreamLayer(conn, clusterPort)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	manager.shutdownFuncs = append(manager.shutdownFuncs, func(ctx context.Context) error {
		return streamLayer.Close()
	})

	transport := raft.NewNetworkTransportWithConfig(&raft.NetworkTransportConfig{
		Stream:  streamLayer,
		MaxPool: 3,
		Timeout: 10 * time.Second,
	})
	manager.shutdownFuncs = append(manager.shutdownFuncs, func(ctx context.Context) error {
		return transport.Close()
	})

	conf := raft.DefaultConfig()
	conf.Logger = hclog.NewNullLogger()
	conf.LocalID = raft.ServerID(conn.selfId)

	manager.inner, err = raft.NewRaft(conf, manager.fsm, store, store, snapshots, transport)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	manager.shutdownFuncs = append(manager.shutdownFuncs, func(ctx context.Context) error {
		future := manager.inner.Shutdown()
		return future.Error()
	})

	conn.setPeerAddedCb(manager.peerAdded)
	conn.setPeerRemovedCb(manager.peerRemoved)

	obsChan := manager.startObservationProcessor()
	observer := raft.NewObserver(obsChan, true, nil)
	manager.inner.RegisterObserver(observer)
	manager.shutdownFuncs = append(manager.shutdownFuncs, func(ctx context.Context) error {
		manager.inner.DeregisterObserver(observer)
		return nil
	})

	// We bootstrap the cluster if we're the only peer.
	if len(conn.peers) == 0 {
		if err := manager.inner.BootstrapCluster(raft.Configuration{
			Servers: []raft.Server{
				{
					ID:      raft.ServerID(conn.selfId),
					Address: raft.ServerAddress(serverAddr),
				},
			},
		}).Error(); err != nil {
			return nil, errors.WithStack(err)
		}
		log.Printf("bootstrapped cluster")
	}

	return manager, nil
}

type inetStreamLayer struct {
	net.Listener
	inner *inetConn
}

func newInetStreamLayer(inner *inetConn, port string) (*inetStreamLayer, error) {
	lis, err := inner.listen("tcp", ":"+port)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return &inetStreamLayer{
		Listener: lis,
		inner:    inner,
	}, nil
}

func (isl *inetStreamLayer) Dial(
	address raft.ServerAddress,
	timeout time.Duration,
) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return isl.inner.dialContext(ctx, "tcp", string(address))
}

var _ raft.StreamLayer = (*inetStreamLayer)(nil)

func (cm *clusterManager) close(ctx context.Context) error {
	if cm.inner.State() == raft.Leader {
		if err := cm.inner.RemoveServer(raft.ServerID(cm.selfId), 0, 0).Error(); err != nil {
			log.Printf("failed to resign from leadership position: %v", err)
		} else {
			log.Println("resigned from leader of cluster")
		}
	}
	for i := len(cm.shutdownFuncs) - 1; i >= 0; i-- {
		if err := cm.shutdownFuncs[i](ctx); err != nil {
			return err
		}
	}
	return cm.errGroup.Wait()
}

func (cm *clusterManager) peerAdded(peer *tailcfg.Node) {
	log.Printf("discovered peer %s (%v)", peer.StableID, peer.Addresses)
	if len(peer.Addresses) > 0 && cm.inner.State() == raft.Leader {
		endpoint := net.JoinHostPort(peer.Addresses[0].Addr().String(), clusterPort)
		if err := cm.inner.AddVoter(raft.ServerID(peer.StableID), raft.ServerAddress(endpoint), 0, 0).Error(); err != nil {
			log.Printf("failed to add voter %s to cluster: %v", peer.StableID, err)
		}
	}
}

func (cm *clusterManager) peerRemoved(peer *tailcfg.Node) {
	log.Printf("lost peer %s", peer.StableID)
	if cm.inner.State() == raft.Leader {
		if err := cm.inner.RemoveServer(raft.ServerID(peer.StableID), 0, 0).Error(); err != nil {
			log.Printf("failed to remove server %s from cluster: %v", peer.StableID, err)
		}
	}
}

func (cm *clusterManager) startObservationProcessor() chan raft.Observation {
	c := make(chan raft.Observation)

	ctx, cancel := context.WithCancel(context.Background())
	cm.errGroup.Go(func() error {
		for {
			select {
			case <-ctx.Done():
				return nil
			case o := <-c:
				cm.processObservation(o)
			}
		}
	})

	cm.shutdownFuncs = append(cm.shutdownFuncs, func(_ context.Context) error {
		cancel()
		return nil
	})

	return c
}

func (cm *clusterManager) processObservation(o raft.Observation) {
	leaderObservation, ok := o.Data.(raft.LeaderObservation)
	if !ok {
		return
	}
	if string(cm.selfId) == string(leaderObservation.LeaderID) {
		log.Printf("elected as leader of cluster (%s)", cm.selfId)
	} else if leaderObservation.LeaderID != "" {
		log.Printf("observed leader as %s", leaderObservation.LeaderID)
	} else {
		log.Println("cluster has no leader")
	}
}

type iterator interface {
	next() bool
	valid() bool
	first() bool
	last() bool
	key() []byte
	value() []byte
	close() error
}

type store interface {
	close() error
	get(ctx context.Context, key []byte) ([]byte, error)
	put(ctx context.Context, key, value []byte) error
	delete(ctx context.Context, key []byte) error
	newIter(ctx context.Context, start, end []byte) (iterator, error)
}

var errNotFound = errors.New("store: not found")

type pebbleStore struct {
	db         *pebble.DB
	syncWrites bool
}

var _ store = (*pebbleStore)(nil)

func newPebbleStore(datadir string, syncWrites bool) (*pebbleStore, error) {
	db, err := pebble.Open(datadir, &pebble.Options{})
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return &pebbleStore{
		db:         db,
		syncWrites: syncWrites,
	}, nil
}

func (s *pebbleStore) close() error {
	return s.db.Close()
}

func (s *pebbleStore) get(_ context.Context, key []byte) ([]byte, error) {
	value, closer, err := s.db.Get(key)
	if errors.Is(err, pebble.ErrNotFound) {
		return nil, errNotFound
	} else if err != nil {
		return nil, errors.WithStack(err)
	}
	defer closer.Close()

	buf := make([]byte, len(value))
	copy(buf, value)

	return buf, nil
}

func (s *pebbleStore) put(_ context.Context, key, value []byte) error {
	opts := pebble.NoSync
	if s.syncWrites {
		opts = pebble.Sync
	}
	if err := s.db.Set(key, value, opts); err != nil {
		return errors.WithStack(err)
	}
	return nil
}

func (s *pebbleStore) delete(_ context.Context, key []byte) error {
	opts := pebble.NoSync
	if s.syncWrites {
		opts = pebble.Sync
	}
	if err := s.db.Delete(key, opts); err != nil {
		return errors.WithStack(err)
	}
	return nil
}

func (s *pebbleStore) newIter(_ context.Context, start, end []byte) (iterator, error) {
	iter := s.db.NewIter(&pebble.IterOptions{
		LowerBound: start,
		UpperBound: end,
	})
	return &pebbleIter{iter: iter}, nil
}

type pebbleIter struct {
	iter *pebble.Iterator
}

var _ iterator = (*pebbleIter)(nil)

func (i *pebbleIter) next() bool    { return i.iter.Next() }
func (i *pebbleIter) valid() bool   { return i.iter.Valid() }
func (i *pebbleIter) first() bool   { return i.iter.First() }
func (i *pebbleIter) last() bool    { return i.iter.Last() }
func (i *pebbleIter) key() []byte   { return i.iter.Key() }
func (i *pebbleIter) value() []byte { return i.iter.Value() }
func (i *pebbleIter) close() error  { return i.iter.Close() }

func deleteRange(store store, start, end []byte) error {
	iter, err := store.newIter(context.Background(), start, end)
	if err != nil {
		return err
	}
	defer iter.close()
	for iter.first(); iter.valid(); iter.next() {
		if err := store.delete(context.Background(), iter.key()); err != nil {
			return err
		}
	}
	return nil
}

func prefixedRange(prefix []byte) ([]byte, []byte) {
	upperBound := func(b []byte) []byte {
		end := make([]byte, len(b))
		copy(end, b)
		for i := len(end) - 1; i >= 0; i-- {
			end[i] = end[i] + 1
			if end[i] != 0 {
				return end[:i+1]
			}
		}
		return nil
	}
	return prefix, upperBound(prefix)
}

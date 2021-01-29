package codanet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	gonet "net"
	"os"
	"path"
	"time"

	dsb "github.com/ipfs/go-ds-badger"
	logging "github.com/ipfs/go-log"
	p2p "github.com/libp2p/go-libp2p"
	p2pconnmgr "github.com/libp2p/go-libp2p-connmgr"
	"github.com/libp2p/go-libp2p-core/connmgr"
	"github.com/libp2p/go-libp2p-core/control"
	"github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/metrics"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	protocol "github.com/libp2p/go-libp2p-core/protocol"
	"github.com/libp2p/go-libp2p-core/routing"
	discovery "github.com/libp2p/go-libp2p-discovery"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p-kad-dht/dual"
	"github.com/libp2p/go-libp2p-peerstore/pstoreds"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	record "github.com/libp2p/go-libp2p-record"
	p2pconfig "github.com/libp2p/go-libp2p/config"
	mdns "github.com/libp2p/go-libp2p/p2p/discovery"
	ma "github.com/multiformats/go-multiaddr"
	"golang.org/x/crypto/blake2b"

	libp2pmplex "github.com/libp2p/go-libp2p-mplex"
	mplex "github.com/libp2p/go-mplex"
)

var (
	logger   = logging.Logger("codanet.Helper")
	gsLogger = logging.Logger("codanet.CodaGatingState")
	NoDHT bool // option for testing to completely disable the DHT

	privateIPs = []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"100.64.0.0/10",
		"198.18.0.0/15",
		"169.254.0.0/16",
	}

	numPeersToExchange = 15 // arbitrary

	pxProtocolID = protocol.ID("/mina/peer-exchange")
)

type getRandomPeersFunc func(num int, from peer.ID) []peer.AddrInfo

func parseCIDR(cidr string) gonet.IPNet {
	_, ipnet, err := gonet.ParseCIDR(cidr)
	if err != nil {
		panic(err)
	}
	return *ipnet
}

type CodaConnectionManager struct {
	ctx context.Context
	host host.Host
	p2pManager   *p2pconnmgr.BasicConnMgr
	minaPeerExchange bool
	getRandomPeers getRandomPeersFunc
	OnConnect    func(network.Network, network.Conn)
	OnDisconnect func(network.Network, network.Conn)
}

func newCodaConnectionManager(maxConnections int, minaPeerExchange bool) *CodaConnectionManager {
	noop := func(net network.Network, c network.Conn) {}

	return &CodaConnectionManager{
		p2pManager:   p2pconnmgr.NewConnManager(25, maxConnections, time.Duration(30*time.Second)),
		OnConnect:    noop,
		OnDisconnect: noop,
		minaPeerExchange: minaPeerExchange,
	}
}

// proxy connmgr.ConnManager interface to p2pconnmgr.BasicConnMgr
func (cm *CodaConnectionManager) TagPeer(p peer.ID, tag string, weight int) {
	cm.p2pManager.TagPeer(p, tag, weight)
}
func (cm *CodaConnectionManager) UntagPeer(p peer.ID, tag string) { cm.p2pManager.UntagPeer(p, tag) }
func (cm *CodaConnectionManager) UpsertTag(p peer.ID, tag string, upsert func(int) int) {
	cm.p2pManager.UpsertTag(p, tag, upsert)
}
func (cm *CodaConnectionManager) GetTagInfo(p peer.ID) *connmgr.TagInfo {
	return cm.p2pManager.GetTagInfo(p)
}
func (cm *CodaConnectionManager) TrimOpenConns(ctx context.Context) { cm.p2pManager.TrimOpenConns(ctx) }
func (cm *CodaConnectionManager) Protect(p peer.ID, tag string)     { cm.p2pManager.Protect(p, tag) }
func (cm *CodaConnectionManager) Unprotect(p peer.ID, tag string) bool {
	return cm.p2pManager.Unprotect(p, tag)
}
func (cm *CodaConnectionManager) IsProtected(p peer.ID, tag string) bool {
	return cm.p2pManager.IsProtected(p, tag)
}
func (cm *CodaConnectionManager) Close() error { return cm.p2pManager.Close() }

// proxy connmgr.Decayer interface to p2pconnmgr.BasicConnMgr (which implements connmgr.Decayer via struct inheritance)
func (cm *CodaConnectionManager) RegisterDecayingTag(name string, interval time.Duration, decayFn connmgr.DecayFn, bumpFn connmgr.BumpFn) (connmgr.DecayingTag, error) {
	// casting to Decayer here should always succeed
	decayer, _ := interface{}(cm.p2pManager).(connmgr.Decayer)
	tag, err := decayer.RegisterDecayingTag(name, interval, decayFn, bumpFn)
	return tag, err
}

// redirect Notifee() to self for notification interception
func (cm *CodaConnectionManager) Notifee() network.Notifiee { return cm }

// proxy Notifee notifications to p2pconnmgr.BasicConnMgr, intercepting Connected and Disconnected
func (cm *CodaConnectionManager) Listen(net network.Network, addr ma.Multiaddr) {
	cm.p2pManager.Notifee().Listen(net, addr)
}
func (cm *CodaConnectionManager) ListenClose(net network.Network, addr ma.Multiaddr) {
	cm.p2pManager.Notifee().ListenClose(net, addr)
}
func (cm *CodaConnectionManager) OpenedStream(net network.Network, stream network.Stream) {
	cm.p2pManager.Notifee().OpenedStream(net, stream)
}
func (cm *CodaConnectionManager) ClosedStream(net network.Network, stream network.Stream) {
	cm.p2pManager.Notifee().ClosedStream(net, stream)
}
func (cm *CodaConnectionManager) Connected(net network.Network, c network.Conn) {
	logger.Debugf("%s connected to %s", c.LocalPeer(), c.RemotePeer())
	cm.OnConnect(net, c)
	cm.p2pManager.Notifee().Connected(net, c)

	if !cm.minaPeerExchange {
		return
	}

	info := cm.GetInfo()
	if len(net.Peers()) <= info.HighWater {
		return
	}
	
	logger.Debugf("node=%s disconnecting from peer=%s; max peers=%d peercount=%d", c.LocalPeer(), c.RemotePeer(), info.HighWater, len(net.Peers()))

	// select random subset of our peers to send over, then disconnect
	if cm.getRandomPeers == nil {
		logger.Error("getRandomPeers function not set")
		return
	}

	peers := cm.getRandomPeers(numPeersToExchange, c.RemotePeer())
	bz, err := json.Marshal(peers)
	if err != nil {
		logger.Error("failed to marshal peers", err)
		return
	}

	stream, err := cm.host.NewStream(cm.ctx, c.RemotePeer(), pxProtocolID)
	if err != nil {
		logger.Error("failed to open stream", err)
		return
	}

	n, err := stream.Write(bz)
	if err != nil {
		logger.Error("failed to write to stream", err)
		return		
	} else if n != len(bz) {
		logger.Error("failed to write all data to stream")
		return		
	}

	logger.Debugf("wrote peers to stream %s", stream.Protocol())
	go func() {
		// small delay to allow for remote peer to read from stream
		time.Sleep(time.Millisecond * 300)
		_ = c.Close()
	}()
}

func (cm *CodaConnectionManager) Disconnected(net network.Network, c network.Conn) {
	cm.OnDisconnect(net, c)
	cm.p2pManager.Notifee().Disconnected(net, c)
}

// proxy remaining p2pconnmgr.BasicConnMgr methods for access
func (cm *CodaConnectionManager) GetInfo() p2pconnmgr.CMInfo {
	return cm.p2pManager.GetInfo()
}

// Helper contains all the daemon state
type Helper struct {
	Host              host.Host
	Mdns              *mdns.Service
	Dht               *dual.DHT
	Ctx               context.Context
	Pubsub            *pubsub.PubSub
	Logger            logging.EventLogger
	Rendezvous        string
	Discovery         *discovery.RoutingDiscovery
	Me                peer.ID
	GatingState       *CodaGatingState
	ConnectionManager *CodaConnectionManager
	BandwidthCounter  *metrics.BandwidthCounter
}

type customValidator struct {
	Base record.Validator
}

// this type implements the ConnectionGating interface
// https://godoc.org/github.com/libp2p/go-libp2p-core/connmgr#ConnectionGating
// the comments of the functions below are taken from those docs.
type CodaGatingState struct {
	logger              logging.EventLogger
	InternalAddrFilters *ma.Filters
	AddrFilters         *ma.Filters
	DeniedPeers         *peer.Set
	AllowedPeers        *peer.Set
}

// NewCodaGatingState returns a new CodaGatingState
func NewCodaGatingState(addrFilters *ma.Filters, denied *peer.Set, allowed *peer.Set) *CodaGatingState {
	if addrFilters == nil {
		addrFilters = ma.NewFilters()
	}

	if denied == nil {
		denied = peer.NewSet()
	}

	if allowed == nil {
		allowed = peer.NewSet()
	}

	internalAddrFilters := ma.NewFilters()
	for _, addr := range privateIPs {
		internalAddrFilters.AddFilter(parseCIDR(addr), ma.ActionDeny)
	}

	logger.Info("computed gating state addr filters: %#v", addrFilters)	

	return &CodaGatingState{
		logger:              gsLogger,
		AddrFilters:         addrFilters,
		InternalAddrFilters: internalAddrFilters,
		DeniedPeers:         denied,
		AllowedPeers:        allowed,
	}
}

func (gs *CodaGatingState) blockedInternalAddr(addr ma.Multiaddr) bool {
	_, exists := os.LookupEnv("CONNECT_PRIVATE_IPS")
	return !exists && gs.InternalAddrFilters.AddrBlocked(addr)
}

func (gs *CodaGatingState) logGate() {
	gs.logger.Debugf("gated a connection with config: %+v", gs)
}

// InterceptPeerDial tests whether we're permitted to Dial the specified peer.
//
// This is called by the network.Network implementation when dialling a peer.
func (gs *CodaGatingState) InterceptPeerDial(p peer.ID) (allow bool) {
	allow = !gs.DeniedPeers.Contains(p) || gs.AllowedPeers.Contains(p)

	if !allow {
		gs.logger.Infof("disallowing peer dial from: %v", p)
		gs.logGate()
	}

	return
}

// InterceptAddrDial tests whether we're permitted to dial the specified
// multiaddr for the given peer.
//
// This is called by the network.Network implementation after it has
// resolved the peer's addrs, and prior to dialling each.
func (gs *CodaGatingState) InterceptAddrDial(id peer.ID, addr ma.Multiaddr) (allow bool) {
	if gs.AddrFilters.AddrBlocked(addr) {
		return false
	}

	allow = gs.AllowedPeers.Contains(id) || (!gs.DeniedPeers.Contains(id) && !gs.AddrFilters.AddrBlocked(addr) && !gs.blockedInternalAddr(addr))

	if !allow {
		gs.logger.Infof("disallowing peer dial from: %v", id)
		gs.logGate()
	}

	return
}

// InterceptAccept tests whether an incipient inbound connection is allowed.
//
// This is called by the upgrader, or by the transport directly (e.g. QUIC,
// Bluetooth), straight after it has accepted a connection from its socket.
func (gs *CodaGatingState) InterceptAccept(addrs network.ConnMultiaddrs) (allow bool) {
	remoteAddr := addrs.RemoteMultiaddr()

	allow = !gs.AddrFilters.AddrBlocked(remoteAddr)

	if !allow {
		gs.logger.Infof("refusing to accept inbound connection from addr: %v", remoteAddr)
		gs.logGate()
	}

	return
}

// InterceptSecured tests whether a given connection, now authenticated,
// is allowed.
//
// This is called by the upgrader, after it has performed the security
// handshake, and before it negotiates the muxer, or by the directly by the
// transport, at the exact same checkpoint.
func (gs *CodaGatingState) InterceptSecured(_ network.Direction, id peer.ID, addrs network.ConnMultiaddrs) (allow bool) {
	// note: we don't care about the direction (inbound/outbound). all
	// connections in coda are symmetric: if i am allowed to connect to
	// you, you are allowed to connect to me.
	remoteAddr := addrs.RemoteMultiaddr()
	allow = gs.AllowedPeers.Contains(id) || !gs.DeniedPeers.Contains(id)

	if !allow {
		gs.logger.Infof("refusing to accept inbound connection from authenticated addr: %v", remoteAddr)
		gs.logGate()
	}

	return
}

// InterceptUpgraded tests whether a fully capable connection is allowed.
//
// At this point, the connection a multiplexer has been selected.
// When rejecting a connection, the gater can return a DisconnectReason.
// Refer to the godoc on the ConnectionGating type for more information.
//
// NOTE: the go-libp2p implementation currently IGNORES the disconnect reason.
func (gs *CodaGatingState) InterceptUpgraded(network.Conn) (allow bool, reason control.DisconnectReason) {
	allow = true
	reason = control.DisconnectReason(0)
	return
}

func (cv customValidator) Validate(key string, value []byte) error {
	log.Printf("DHT Validating: %s = %s", key, value)
	return cv.Base.Validate(key, value)
}

func (cv customValidator) Select(key string, values [][]byte) (int, error) {
	log.Printf("DHT Selecting Among: %s = %s", key, bytes.Join(values, []byte("; ")))
	return cv.Base.Select(key, values)
}

func (h *Helper) getRandomPeers(num int, from peer.ID) []peer.AddrInfo {
	peers := h.Host.Peerstore().Peers()
	if len(peers)-2 < num {
		num = len(peers)-2 // -2 because excluding ourself and the peer we are sending this to
	}

	ret := make([]peer.AddrInfo, num)
	for i := 0; i < num; i++ {
		var idx int
		for {
			idx = rand.Intn(len(peers))
			if peers[idx] != h.Host.ID() && peers[idx] != from {
				break
			}
		}

		ret[i] = h.Host.Peerstore().PeerInfo(peers[idx])
		if idx != len(peers)-1 {
			peers = append(peers[:idx], peers[idx+1:]...)
		} else {
			peers = peers[:len(peers)-1]
		}
	}

	logger.Debugf("node=%s sending random peers", h.Host.ID(), ret)
	return ret
}

func (h *Helper) handlePxStreams(s network.Stream) {
	for {
		dec := json.NewDecoder(s)
		peers := []peer.AddrInfo{}
		err := dec.Decode(&peers)
		if err != nil && err == io.EOF {
			continue
		} else if err != nil && err.Error() == "stream reset" {
			return
		} else if err != nil {
			logger.Errorf("failed to decode list of peers err=%s", err)
			continue
		}

		for _, peer := range peers {
			go func() {
				err = h.Host.Connect(h.Ctx, peer)
				if err != nil {
					logger.Errorf("failed to connect to peer err=%s", err)
				}
				logger.Debugf("connected to peer! %s", peer)
			}()
		}

		_ = s.Close()
	}
}

// TODO: just put this into main.go?

// MakeHelper does all the initialization to run one host
func MakeHelper(ctx context.Context, listenOn []ma.Multiaddr, externalAddr ma.Multiaddr, statedir string, pk crypto.PrivKey, networkID string, seeds []peer.AddrInfo, gatingState *CodaGatingState, maxConnections int, minaPeerExchange bool) (*Helper, error) {
	me, err := peer.IDFromPrivateKey(pk)
	if err != nil {
		return nil, err
	}

	dso := dsb.DefaultOptions

	ds, err := dsb.NewDatastore(path.Join(statedir, "libp2p-peerstore-v0"), &dso)
	if err != nil {
		return nil, err
	}

	dsoDht := dsb.DefaultOptions
	dsDht, err := dsb.NewDatastore(path.Join(statedir, "libp2p-dht-v0"), &dsoDht)
	if err != nil {
		return nil, err
	}

	ps, err := pstoreds.NewPeerstore(ctx, ds, pstoreds.DefaultOpts())
	if err != nil {
		return nil, err
	}

	rendezvousString := fmt.Sprintf("/coda/0.0.1/%s", networkID)

	pnetKey := blake2b.Sum256([]byte(rendezvousString))

	// custom validator to omit the ipns validation.
	rv := customValidator{Base: record.NamespacedValidator{"pk": record.PublicKeyValidator{}}}

	var kad *dual.DHT

	mplex.MaxMessageSize = 1 << 30

	connManager := newCodaConnectionManager(maxConnections, minaPeerExchange)
	bandwidthCounter := metrics.NewBandwidthCounter()

	host, err := p2p.New(ctx,
		p2p.Muxer("/coda/mplex/1.0.0", libp2pmplex.DefaultTransport),
		p2p.Identity(pk),
		p2p.Peerstore(ps),
		p2p.DisableRelay(),
		p2p.ConnectionGater(gatingState),
		p2p.ConnectionManager(connManager),
		p2p.ListenAddrs(listenOn...),
		p2p.AddrsFactory(func(as []ma.Multiaddr) []ma.Multiaddr {
			if externalAddr != nil {
				as = append(as, externalAddr)
			}

			fs := ma.NewFilters()
			for _, addr := range privateIPs {
				fs.AddFilter(parseCIDR(addr), ma.ActionDeny)
			}

			bs := []ma.Multiaddr{}
			for _, a := range as {
				if fs.AddrBlocked(a) {
					continue
				}
				bs = append(bs, a)
			}

			return bs
		}),
		p2p.NATPortMap(),
		p2p.Routing(
			p2pconfig.RoutingC(func(host host.Host) (routing.PeerRouting, error) {
				if NoDHT {
					return nil, nil
				}

				kad, err = dual.New(ctx, host,
					dual.WanDHTOption(dht.Datastore(dsDht)),
					dual.DHTOption(dht.Validator(rv)),
					dual.DHTOption(dht.BootstrapPeers(seeds...)),
					dual.DHTOption(dht.ProtocolPrefix("/coda")),
				)
				return kad, err
			})),
		p2p.UserAgent("github.com/codaprotocol/coda/tree/master/src/app/libp2p_helper"),
		p2p.PrivateNetwork(pnetKey[:]),
		p2p.BandwidthReporter(bandwidthCounter),
	)

	if err != nil {
		return nil, err
	}

	// nil fields are initialized by beginAdvertising
	h := &Helper{
		Host:              host,
		Ctx:               ctx,
		Mdns:              nil,
		Dht:               kad,
		Pubsub:            nil,
		Logger:            logger,
		Rendezvous:        rendezvousString,
		Discovery:         nil,
		Me:                me,
		GatingState:       gatingState,
		ConnectionManager: connManager,
		BandwidthCounter:  bandwidthCounter,
	}

	if !minaPeerExchange {
		return h, nil
	}

	connManager.getRandomPeers = h.getRandomPeers
	connManager.ctx = ctx
	connManager.host = host
	h.Host.SetStreamHandler(pxProtocolID, h.handlePxStreams)
	return h, nil
}

package discovery

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/grandcat/zeroconf"
	"notashelf.dev/ncro/internal/config"
	"notashelf.dev/ncro/internal/prober"
)

// Tracks discovered nix-serve instances and maintains the upstream list.
type Discovery struct {
	cfg              config.DiscoveryConfig
	prober           *prober.Prober
	resolver         *zeroconf.Resolver
	discovered       map[string]*discoveredPeer
	mu               sync.RWMutex
	stopCh           chan struct{}
	stopOnce         sync.Once
	waitGroup        sync.WaitGroup
	onAddUpstream    func(url string, priority int)
	onRemoveUpstream func(url string)
}

type discoveredPeer struct {
	url      string
	lastSeen time.Time
}

// Creates a new Discovery manager.
func New(cfg config.DiscoveryConfig, p *prober.Prober) (*Discovery, error) {
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return nil, fmt.Errorf("create zeroconf resolver: %w", err)
	}

	return &Discovery{
		cfg:        cfg,
		prober:     p,
		resolver:   resolver,
		discovered: make(map[string]*discoveredPeer),
		stopCh:     make(chan struct{}),
	}, nil
}

// Sets callbacks for upstream addition/removal. These are invoked when peers
// are discovered or leave the network.
func (d *Discovery) SetCallbacks(
	add func(url string, priority int),
	remove func(url string),
) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onAddUpstream = add
	d.onRemoveUpstream = remove
}

// Starts browsing for services on the local network. Blocks until the context
// is cancelled or Stop is called.
func (d *Discovery) Start(ctx context.Context) error {
	entries := make(chan *zeroconf.ServiceEntry)

	d.waitGroup.Add(1)
	go d.handleEntries(ctx, entries)

	d.waitGroup.Add(1)
	go d.maintainPeers(ctx)

	if err := d.resolver.Browse(ctx, d.cfg.ServiceName, d.cfg.Domain, entries); err != nil {
		close(entries)
		d.stopOnce.Do(func() { close(d.stopCh) })
		d.waitGroup.Wait()
		return fmt.Errorf("browse services: %w", err)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-d.stopCh:
		return nil
	}
}

// Stops the discovery process.
func (d *Discovery) Stop() {
	d.stopOnce.Do(func() { close(d.stopCh) })
	d.waitGroup.Wait()
}

// Processes discovered service entries.
func (d *Discovery) handleEntries(ctx context.Context, entries chan *zeroconf.ServiceEntry) {
	defer d.waitGroup.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case <-d.stopCh:
			return
		case entry, ok := <-entries:
			if !ok {
				return
			}
			d.handleEntry(ctx, entry)
		}
	}
}

// Handles a single service entry.
func (d *Discovery) handleEntry(_ context.Context, entry *zeroconf.ServiceEntry) {
	if len(entry.AddrIPv4) == 0 && len(entry.AddrIPv6) == 0 {
		slog.Debug("discovered service has no addresses", "instance", entry.Instance)
		return
	}

	var addr string
	if len(entry.AddrIPv4) > 0 {
		addr = entry.AddrIPv4[0].String()
	} else {
		addr = entry.AddrIPv6[0].String()
	}

	url := "http://" + net.JoinHostPort(addr, fmt.Sprintf("%d", entry.Port))
	key := fmt.Sprintf("%s@%s", entry.Instance, entry.HostName)

	d.mu.Lock()
	defer d.mu.Unlock()

	// Check if we already know this peer
	if _, exists := d.discovered[key]; exists {
		d.discovered[key].lastSeen = time.Now()
		return
	}

	// New peer discovered
	slog.Info("discovered nix-serve instance", "instance", entry.Instance, "url", url)

	d.discovered[key] = &discoveredPeer{
		url:      url,
		lastSeen: time.Now(),
	}

	// Notify callback if set
	if d.onAddUpstream != nil {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("panic in add upstream callback", "recover", r)
				}
			}()
			d.onAddUpstream(url, d.cfg.Priority)
		}()
	}
}

// Removes peers that haven't been seen within the TTL period.
func (d *Discovery) maintainPeers(ctx context.Context) {
	defer d.waitGroup.Done()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-d.stopCh:
			return
		case <-ticker.C:
			d.cleanupPeers()
		}
	}
}

// Cleans up stale peer entries.
func (d *Discovery) cleanupPeers() {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()
	// TTL is the discovery response time; peers should re-announce periodically.
	// Use 3x TTL as the expiration window.
	expiration := d.cfg.DiscoveryTime.Duration * 3
	if expiration == 0 {
		expiration = time.Second
	}

	for key, peer := range d.discovered {
		if now.Sub(peer.lastSeen) > expiration {
			slog.Info("removing stale peer", "url", peer.url)
			delete(d.discovered, key)
			if d.onRemoveUpstream != nil {
				go func(url string) {
					defer func() {
						if r := recover(); r != nil {
							slog.Error("panic in remove upstream callback", "recover", r)
						}
					}()
					d.onRemoveUpstream(url)
				}(peer.url)
			}
		}
	}
}

// Returns a list of currently discovered peer URLs.
func (d *Discovery) DiscoveredPeers() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()

	peers := make([]string, 0, len(d.discovered))
	for _, peer := range d.discovered {
		peers = append(peers, peer.url)
	}
	return peers
}

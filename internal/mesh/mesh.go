package mesh

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/vmihailenco/msgpack/v5"
	"notashelf.dev/ncro/internal/cache"
)

// Gossip message types.
type MsgType uint8

const (
	MsgAnnounce MsgType = 1
)

// Wire format for gossip messages.
type Message struct {
	Type      MsgType
	NodeID    string
	Timestamp int64
	Routes    []cache.RouteEntry
}

// Cryptographic identity of an ncro node.
type Node struct {
	pubKey  ed25519.PublicKey
	privKey ed25519.PrivateKey
	store   *RouteStore
}

// Loads or generates an ed25519 keypair from keyPath.
// Pass "" for an ephemeral in-memory key.
func NewNode(keyPath string, store *RouteStore) (*Node, error) {
	if store == nil {
		store = NewRouteStore()
	}
	if keyPath == "" {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("generate key: %w", err)
		}
		return &Node{pubKey: pub, privKey: priv, store: store}, nil
	}
	data, err := os.ReadFile(keyPath)
	if err == nil && len(data) == ed25519.PrivateKeySize {
		priv := ed25519.PrivateKey(data)
		return &Node{pubKey: priv.Public().(ed25519.PublicKey), privKey: priv, store: store}, nil
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	if err := os.WriteFile(keyPath, priv, 0600); err != nil {
		return nil, fmt.Errorf("write key: %w", err)
	}
	return &Node{pubKey: pub, privKey: priv, store: store}, nil
}

// Returns the hex-encoded public key fingerprint.
func (n *Node) ID() string {
	return hex.EncodeToString(n.pubKey[:8])
}

// Returns the node's public key.
func (n *Node) PublicKey() ed25519.PublicKey {
	return n.pubKey
}

// Serializes msg with msgpack and signs it; returns (data, signature, error).
func (n *Node) Sign(msg Message) ([]byte, []byte, error) {
	data, err := msgpack.Marshal(msg)
	if err != nil {
		return nil, nil, err
	}
	return data, ed25519.Sign(n.privKey, data), nil
}

// Checks that sig is a valid ed25519 signature of data under pubKey.
func Verify(pubKey ed25519.PublicKey, data, sig []byte) error {
	if !ed25519.Verify(pubKey, data, sig) {
		return errors.New("invalid signature")
	}
	return nil
}

// In-memory route table with merge-conflict resolution for gossip.
type RouteStore struct {
	mu     sync.RWMutex
	routes map[string]*cache.RouteEntry
}

// Creates an empty RouteStore.
func NewRouteStore() *RouteStore {
	return &RouteStore{routes: make(map[string]*cache.RouteEntry)}
}

// Applies incoming routes: lower latency wins; newer LastVerified wins on tie.
func (rs *RouteStore) Merge(incoming []cache.RouteEntry) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	now := time.Now()
	for _, r := range incoming {
		r := r
		if r.TTL.Before(now) {
			continue
		}
		existing, ok := rs.routes[r.StorePath]
		if !ok {
			rs.routes[r.StorePath] = &r
			continue
		}
		if r.LatencyEMA < existing.LatencyEMA {
			rs.routes[r.StorePath] = &r
		} else if r.LatencyEMA == existing.LatencyEMA && r.LastVerified.After(existing.LastVerified) {
			rs.routes[r.StorePath] = &r
		}
	}
}

// Returns a copy of the stored route, or nil.
func (rs *RouteStore) Get(storePath string) *cache.RouteEntry {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	e, ok := rs.routes[storePath]
	if !ok {
		return nil
	}
	cp := *e
	return &cp
}

// Returns up to n routes for sync batching.
func (rs *RouteStore) Top(n int) []cache.RouteEntry {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	result := make([]cache.RouteEntry, 0, min(n, len(rs.routes)))
	for _, e := range rs.routes {
		result = append(result, *e)
		if len(result) >= n {
			break
		}
	}
	return result
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

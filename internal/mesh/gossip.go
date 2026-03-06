package mesh

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/vmihailenco/msgpack/v5"
	"notashelf.dev/ncro/internal/cache"
)

const (
	maxPacketSize = 65536                                         // UDP max payload
	headerSize    = ed25519.PublicKeySize + ed25519.SignatureSize // 32 + 64 = 96
)

// Wire format: [32-byte sender pubkey][64-byte ed25519 sig][msgpack body]

func encodePacket(node *Node, msg Message) ([]byte, error) {
	body, sig, err := node.Sign(msg)
	if err != nil {
		return nil, err
	}
	pkt := make([]byte, headerSize+len(body))
	copy(pkt[:ed25519.PublicKeySize], node.PublicKey())
	copy(pkt[ed25519.PublicKeySize:headerSize], sig)
	copy(pkt[headerSize:], body)
	return pkt, nil
}

func decodePacket(pkt []byte) (pubKey ed25519.PublicKey, sig, body []byte, msg Message, err error) {
	if len(pkt) < headerSize {
		return nil, nil, nil, Message{}, fmt.Errorf("packet too short: %d bytes", len(pkt))
	}
	pubKey = ed25519.PublicKey(pkt[:ed25519.PublicKeySize])
	sig = pkt[ed25519.PublicKeySize:headerSize]
	body = pkt[headerSize:]
	if err := msgpack.Unmarshal(body, &msg); err != nil {
		return nil, nil, nil, Message{}, fmt.Errorf("unmarshal: %w", err)
	}
	return pubKey, sig, body, msg, nil
}

// Starts a UDP listener at addr. All messages are signature-verified.
// When allowedKeys is non-empty, messages from unlisted senders are dropped.
// Pass no keys (or an empty list) to accept messages from any sender.
func ListenAndServe(addr string, store *RouteStore, allowedKeys ...ed25519.PublicKey) error {
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	go func() {
		defer conn.Close()
		buf := make([]byte, maxPacketSize)
		for {
			n, src, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			pubKey, sig, body, msg, err := decodePacket(buf[:n])
			if err != nil {
				slog.Warn("mesh: malformed packet", "src", src, "error", err)
				continue
			}
			if len(allowedKeys) > 0 {
				allowed := false
				for _, k := range allowedKeys {
					if bytes.Equal(k, pubKey) {
						allowed = true
						break
					}
				}
				if !allowed {
					slog.Warn("mesh: rejecting packet from unknown sender", "src", src)
					continue
				}
			}
			if err := Verify(pubKey, body, sig); err != nil {
				slog.Warn("mesh: signature verification failed", "src", src, "error", err)
				continue
			}
			if msg.Type == MsgAnnounce && len(msg.Routes) > 0 {
				store.Merge(msg.Routes)
				slog.Debug("mesh: merged peer routes", "node", msg.NodeID, "src", src, "count", len(msg.Routes))
			}
		}
	}()
	return nil
}

// Sends an MsgAnnounce carrying routes to a single peer address.
func Announce(peerAddr string, node *Node, routes []cache.RouteEntry) error {
	msg := Message{
		Type:      MsgAnnounce,
		NodeID:    node.ID(),
		Timestamp: time.Now().UnixNano(),
		Routes:    routes,
	}
	pkt, err := encodePacket(node, msg)
	if err != nil {
		return err
	}
	addr, err := net.ResolveUDPAddr("udp", peerAddr)
	if err != nil {
		return err
	}
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err = conn.Write(pkt)
	return err
}

// RouteSource retrieves routes to gossip.
type RouteSource interface {
	ListRecentRoutes(n int) ([]cache.RouteEntry, error)
}

// Announces our top routes to each peer on interval. Blocks until stop is closed.
func RunGossipLoop(node *Node, src RouteSource, peers []string, interval time.Duration, stop <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			routes, err := src.ListRecentRoutes(100)
			if err != nil {
				slog.Warn("mesh: failed to list routes for gossip", "error", err)
				continue
			}
			if len(routes) == 0 {
				continue
			}
			for _, peer := range peers {
				if err := Announce(peer, node, routes); err != nil {
					slog.Warn("mesh: announce failed", "peer", peer, "error", err)
				}
			}
			slog.Debug("mesh: announced routes to peers", "routes", len(routes), "peers", len(peers))
		}
	}
}

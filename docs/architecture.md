# Architecture

`ncro` is a Nix cache router. It sits in front of one or more upstream caches,
learns which upstream answers fastest for a given path, and reuses that decision
until it expires.

```mermaid
flowchart LR
  client[Nix client] --> proxy[ncro]
  proxy --> info[narinfo request]
  proxy --> nar[NAR stream]
  info --> cache[(SQLite route cache)]
  info --> race[Parallel upstream race]
  race --> chosen[Chosen upstream]
  chosen --> cache
  nar --> chosen
```

The routing path is simple: a narinfo lookup first checks SQLite, then falls
back to a parallel race across upstreams when there is no usable entry. The
winning upstream is stored with a TTL, so later requests can skip the race.

```mermaid
sequenceDiagram
  participant C as Client
  participant N as ncro
  participant S as SQLite
  participant U as Upstreams

  C->>N: GET /<hash>.narinfo
  N->>S: lookup route
  alt cache hit
    S-->>N: upstream URL
    N->>U: fetch narinfo or NAR
  else cache miss
    N->>U: race requests in parallel
    U-->>N: first success wins
    N->>S: store route
  end
  N-->>C: response
```

Background health probes keep latency estimates current by calling
`HEAD /nix-cache-info` on a timer. The health layer uses EMA smoothing, so a
single bad probe does not immediately dominate the routing decision.

```mermaid
flowchart TD
  probe[Background probe loop] --> head[HEAD /nix-cache-info]
  head --> ema[EMA latency update]
  ema --> status[Health state]
  status --> router[Router ordering]
```

Selection is driven by latency first. When two upstreams are effectively tied,
`priority` breaks the tie. The router also tracks failures and probe volume so it
can distinguish a briefly slow cache from one that is trending unhealthy.

Persistence is intentionally narrow. SQLite stores route decisions and health
snapshots so a restart does not force ncro to relearn everything from scratch.

Discovery and mesh are optional extensions. Discovery can add peers from the
local network, while mesh gossip shares recent route decisions across trusted
nodes using signed UDP packets.

```mermaid
flowchart LR
  subgraph optional[Optional coordination]
    discovery[Discovery] --> peers[Peer set]
    mesh[Mesh gossip] --> peers
    peers --> router[Routing decisions]
  end
```

At runtime, ncro loads config, validates it, opens SQLite, seeds health state,
starts background loops, and finally binds the HTTP listener. Shutdown is driven
by the normal process termination path and background work is told to stop
gracefully.

## Configuration Reference

The most important settings are `upstreams`, `server.listen`, `cache.db_path`,
`cache.ttl`, `cache.negative_ttl`, `cache.latency_alpha`, `server.cache_priority`,
`discovery.enabled`, and `mesh.enabled`.

`upstreams` defines the cache backends ncro can use. Each upstream can carry a
`priority` value and an optional `public_key` for mesh verification.

`cache.ttl` is how long a successful routing decision remains trusted. The
negative TTL applies to failed lookups so ncro does not immediately retry the
same miss.

`cache.latency_alpha` controls how quickly EMA latency reacts to new probes. A
smaller value smooths jitter; a larger value reacts faster to recent changes.

`server.cache_priority` is used when the server layer needs to compare cache
responses. It should stay positive.

`discovery.enabled` and `mesh.enabled` turn on the optional network-coordination
paths described above. Discovery is opportunistic; mesh is signed and intended
for trusted peers.

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
    N->>N: parse narinfo and apply upstream filters
    alt accepted
      N->>S: store route
    else rejected
      N->>U: continue with remaining candidates
    end
  end
  N-->>C: response
```

Path filters sit after the narinfo fetch, not before the upstream race. A Nix
client requests `/<hash>.narinfo`, so ncro only knows the store hash at the start
of routing. The full `StorePath`, references, and deriver are inside the narinfo
body. This means filtering is a validation step for candidate winners: if a
winning upstream's narinfo is rejected, the route is not cached and ncro retries
the remaining candidates.

NAR streaming, on another hand, follows a different path. There is actually no
race and when a client requests `/nar/<hash>.nar`, ncro looks up the route for
the corresponding narinfo hash. _If_ a route exists, it opens a connection to
the winning upstream and streams the response body directly to the client
without buffering to disk. If no route exists, it tries upstreams in latency
order, falling through on 404 until one succeeds.

```mermaid
sequenceDiagram
  participant C as Client
  participant N as ncro
  participant U as Upstreams

  C->>N: GET /nar/<hash>.nar
  N->>U: try upstreams in latency order
  alt upstream has the NAR
    U-->>N: 200 + stream
    N-->>C: stream (zero copy)
  else upstream returns 404
    N->>U: try next upstream
  end
```

Background health probes keep latency estimates current by calling
`HEAD /nix-cache-info` every 30 seconds. The health layer uses Exponentially
Weighted Moving Average (EMA) smoothing, so a single bad probe does not
immediately dominate the routing decision:

$$
L_t = \alpha \cdot R_t + (1 - \alpha) \cdot L_{t-1}
$$

Where $R_t$ is the latest observed latency, $L_t$ is the new estimate, and
`alpha` (`cache.latency_alpha`, default `0.3`) controls how quickly the estimate
adapts. Higher values react faster to real changes; lower values filter out
noise.

```mermaid
flowchart TD
  probe[Background probe loop] --> head[HEAD /nix-cache-info]
  head --> ema[EMA latency update]
  ema --> status[Health state]
  status --> router[Router ordering]
```

Selection is driven by latency first. When two upstreams are effectively tied,
`priority` breaks the tie. The router also tracks failures and probe volume so
it can distinguish a briefly slow cache from one that is trending unhealthy.
Per-upstream filters can reject a candidate winner after its narinfo is fetched;
this prevents project-specific caches from becoming winners for unrelated paths.

> [!TIP]
> Persistence is intentionally narrow. SQLite stores two kinds of data so a
> restart does not force ncro to relearn everything from scratch.

First type of stored data is **route entries**, a mapping from narinfo hash to
the winning upstream URL, stored with a creation timestamp and TTL. When the
cache exceeds `max_entries`, the least recently used entry is evicted first.
**Health snapshots** on another hand are per-upstream EMA latency estimates and
failure counts, refreshed by the background probe loop.

Discovery and mesh are optional extensions. Discovery can add peers from the
local network, while mesh gossip shares recent route decisions across trusted
nodes using signed UDP packets. Consider:

```mermaid
flowchart LR
  subgraph optional[Optional coordination]
    discovery[Discovery] --> peers[Peer set]
    mesh[Mesh gossip] --> peers
    peers --> router[Routing decisions]
  end
```

At runtime, ncro loads config, validates it, opens SQLite, seeds health state,
starts background loops, and finally binds the HTTP listener. The HTTP server
and all background tasks run on tokio's async runtime, allowing concurrent
upstream connections without thread-per-connection overhead. Shutdown is driven
by the normal process termination path and background work is told to stop
gracefully.

## Configuration Reference

The most important settings are `upstreams`, `server.listen`, `cache.db_path`,
`cache.ttl`, `cache.negative_ttl`, `cache.latency_alpha`,
`server.cache_priority`, `discovery.enabled`, `discovery.address_family`, and `mesh.enabled`.

`upstreams` defines the cache backends ncro can use. Each upstream can carry a
`priority` value, optional Basic Auth credentials, an optional `public_key` for
narinfo signature verification, and optional `filters`.

Upstream filters support `allow` and `deny` rules over narinfo fields. The
available fields are `name`, `store_path`, `reference`, and `deriver`; patterns
use `*` wildcards. Deny rules always win. If any allow rules are configured for
an upstream, at least one must match for that upstream to be accepted.

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

`discovery.address_family` controls which addresses from an mDNS-discovered
peer are registered as upstreams. The default `any` registers all routable
addresses (IPv4 and IPv6) so the race engine can try them in parallel. Set
`ipv4` or `ipv6` when the upstream server only listens on one address family.

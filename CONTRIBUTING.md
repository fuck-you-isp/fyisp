# Contributing

Everything is built and tested in Docker; you don't need Go installed. This file covers the endpoints fyisp measures. For code changes, read [AGENTS.md](AGENTS.md): the repo map, every build and test command, and the project's conventions.

## Adding or fixing endpoints

The endpoints fyisp can measure live in [`internal/profile/catalog/`](internal/profile/catalog/), one YAML file per provider. The file format is described in [`SCHEMA.md`](internal/profile/catalog/SCHEMA.md). Named profiles (`--profile clouds,gaming`) are selections over the catalog, defined in [`internal/profile/profiles.yml`](internal/profile/profiles.yml). New endpoints ship with the next fyisp release.

Good pull requests:

- **A new provider:** one new file listing every location the provider offers, with the official location page in `sources`.
- **A missing region:** one target added to the existing file.
- **A dead endpoint:** replace the endpoint with a working one. If none exists, set `kinds: []` and `tags: [unreachable]` and say why in `notes`.
- **A wrong location:** fix `city`, `country` or `geo`, and say how you checked it.

### Endpoint rules

1. **Choose endpoints that are fine to measure,** in this order of preference:
   1. a speed-test, looking-glass or "ping" host the provider publishes for each location;
   2. a public regional API or object-storage endpoint;
   3. a health path.

   Don't use login or account pages. Don't use anything whose terms forbid automated checks.
2. **It must accept TCP connections.** fyisp measures catalog targets with a TCP connect: every running fyisp opens one connection per target every 15 s (handshake, then an immediate close, no data sent). The endpoint needs a TCP listener on `port` (default 443). Replies to ping or HTTP requests are not used. (STUN servers are the exception: see [UDP (STUN) targets](#udp-stun-targets).)
3. **No authentication.** Don't use API keys, account IDs or requests that create or change anything.
4. **Check the location.** A regional target must really be in that city. Checks include:
   - it resolves to a unicast address;
   - rDNS and IP geolocation agree with the claimed city;
   - the round-trip time is plausible from where you are.

   Global or anycast fronts are allowed, but mark them `anycast: true`.
5. **Record only what you verified.**
   - `kinds: [tcp]` once a TCP connect to `port` worked in your own checks; `kinds: []` if it did not.
   - Fill in `verified` with the date and `tcp: true`.
   - A host that listens only on another port (e.g. HTTP only) gets that `port`, e.g. `port: 80`.
6. **Never rename existing targets.** A target's name identifies its stored history. New names must be unique across all files, use `[A-Za-z0-9._-]` with at most 40 characters, and follow the `<Provider>-<location>` pattern.

When checking an endpoint, keep it light: a few TCP connects per endpoint. For example:

```sh
nc -vz -w 3 host.example 443
```

### UDP (STUN) targets

Targets with `kinds: [udp]` measure the UDP path of calls with STUN (format in [`SCHEMA.md`](internal/profile/catalog/SCHEMA.md#udp-stun-targets)). They follow the rules above, plus:

- **STUN Binding only.** fyisp sends 20-byte Binding Requests, a burst of 5 every 5 s (or the target's `interval`, e.g. 30 s for regional servers). Never TURN, never an allocation, no credentials.
- **Documented public STUN only.** The vendor publishes the server for clients to use without credentials, or its own clients and network test tools use it unauthenticated. No community STUN lists, no media ports that only work inside a call.
- **Check it with a couple of Binding Requests** and record the result in `verified`, e.g. `{date: 2026-10-10, stun: 2/2, rtt_ms: 26.4}`.
- **Tags:** `udp-path` on every STUN target, plus `app:<name>` (e.g. `app:teams`) when it stands for one app's media path.
- **Add it to [docs/STUN-TERMS.md](docs/STUN-TERMS.md)** with a link to the vendor's documentation. Undocumented hosts are not shipped.

A STUN server sees your public IP address, as every TCP target does.

### Before opening the pull request

```sh
docker build --target test .
```

This runs every catalog check:
- `TestEmbeddedCatalog` rejects malformed files: unknown fields, duplicate names, bad `geo`, `kinds` (anything but `[tcp]`, `[udp]` or `[]`) or `country` values, udp targets without a `port`, and private addresses.
- `TestBuiltinProfiles` checks that every profile still resolves.
- `TestTilesCatalog` checks that every target with a `tile:` tag is listed by its tile in [`tiles.go`](internal/profile/tiles.go).
- `TestDefaultMatchesCatalog` checks that every target in [`default.yml`](internal/profile/default.yml) matches its catalog entry.
- `TestShippedProfilesTCPOnly` and `TestShippedUDP` check that shipped profiles use TCP only, apart from the pinned STUN targets.

If you change a target in `default.yml` or a `tile:` tag, the Home goldens change too: regenerate them as described in [AGENTS.md](AGENTS.md#build-and-test-docker-only).

To see what a profile selects:

```sh
docker build -t fyisp-dev . && docker run --rm fyisp-dev profiles aws
```

## Speed-test servers (iperf3)

The public iperf3 servers fyisp uses for speed tests live in [`internal/profile/catalog/iperf3/`](internal/profile/catalog/iperf3/), one file per provider, plus `regions.yml`. Only servers from a published public list are used, and each operator needs a row in [docs/IPERF3-TERMS.md](docs/IPERF3-TERMS.md). The steps and the tests that guard them are in [AGENTS.md](AGENTS.md#catalog-profiles-and-speed-test-servers).

## Code changes

Open an issue first for anything larger than a fix, so we can agree on the approach. Keep pull requests focused, run the test target, and add a test for the behaviour you change. [AGENTS.md](AGENTS.md) has the commands and conventions. Security issues go through [SECURITY.md](SECURITY.md), not public issues.

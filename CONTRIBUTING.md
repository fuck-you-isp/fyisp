# Contributing

Everything is built and tested in Docker; you don't need Go installed.

```sh
docker build --target test .                      # gofmt, vet, go test -race
docker build --target dist --output type=local,dest=dist .   # release binaries
```

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
2. **It must accept TCP connections.** fyisp measures catalog targets with a TCP connect only: every running fyisp opens one connection per target every 15 s (handshake, then an immediate close, no data sent). The endpoint needs a TCP listener on `port` (default 443). Replies to ping or HTTP requests are not used.
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

### Before opening the pull request

```sh
docker build --target test .
```

`TestEmbeddedCatalog` rejects malformed files: unknown fields, duplicate names, bad `geo`, `kinds` (anything but `[tcp]` or `[]`) or `country` values, and private addresses. `TestBuiltinProfiles` checks that every profile still resolves.

To see what a profile selects:

```sh
docker build --target source -t fyisp-src . && docker run --rm fyisp-src go run ./cmd/fyisp profiles aws
```

## Code changes

Open an issue first for anything larger than a fix, so we can agree on the approach. Keep pull requests focused, run the test target, and add a test for the behaviour you change. Security issues go through [SECURITY.md](SECURITY.md), not public issues.

# Storage tests

Storage tests check the S3-compatible endpoints you use: whether fyisp can reach each one, how long each step of a connection takes, and what to fix when one refuses. They work with any S3-compatible endpoint (a cloud provider's, or a MinIO, Garage or NAS on your network) and nothing in fyisp is specific to a vendor or a region. Open them in **Details → Storage**. The API is in [STORAGE-API.md](STORAGE-API.md).

## Targets

A target is one endpoint you save:

- **Name**: your label for it.
- **Endpoint**: `https://host` or `https://host:port`, as your provider lists it ("S3 endpoint", "S3 API URL" or "S3-compatible endpoint"). `http://` works only for a server on your own network (a private or loopback address), so keys are never signed over plain HTTP across the internet.
- **Region**: as your provider names it (`us-east-1`, `auto`, or the server's own name). It is part of every signature; empty means `us-east-1`.
- **Bucket** (optional): the bucket a check asks for. Without one, a check asks for the endpoint itself.
- **Bucket address**: *in the host name* (`bucket.host`, most clouds) or *in the path* (`host/bucket`, most self-hosted servers, and any endpoint given as an IP address). A bucket name with dots needs *in the path* over HTTPS: the dots break the certificate check otherwise.
- **Uses its own certificate authority**: for a self-hosted server whose certificate your system doesn't trust, paste the certificate authority's PEM. fyisp then trusts that authority for this target only. It never skips the certificate check.

When you save a target, fyisp looks its name up and notes whether it is on your network or on the internet. Later connections must go to an address of the same kind, so a name can't be switched to point somewhere else (DNS rebinding). fyisp never connects to link-local addresses (cloud metadata), multicast, reserved, benchmarking or documentation addresses. IPv6 isn't used.

Targets are kept in `storage-targets.json` in the data directory (`fyisp paths` shows where), readable by fyisp's user only. It is not part of the database: back it up with the database if you want to keep your targets.

## Check

**Check** sends one unsigned request to the target (a `HEAD` of the bucket, or of the endpoint) and shows how it went: whether it connected, the time for the name lookup, the connection, encryption and the first byte, and the HTTP status it answered. Any answer shows the endpoint is up: without keys, an "access denied" (403) is what a private bucket says, and a 404 or a redirect can mean the bucket isn't at this endpoint or in this region. When encryption fails, it says what to fix: the bucket address (a bucket name with dots) or the certificate authority. A check moves almost no data and stores only its result, in memory, until fyisp restarts or the target's settings change.

Targets never hold keys. A signed check, with keys sent along with it, is available through the API ([STORAGE-API.md](STORAGE-API.md)); it needs only permission to list the bucket under `fyisp-test/`, so use keys scoped to a test bucket, or short-lived session credentials.

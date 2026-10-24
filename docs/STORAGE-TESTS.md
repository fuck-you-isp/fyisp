# Storage tests

Storage tests check the S3-compatible endpoints you use: whether fyisp can reach each one, how long each step of a connection takes, and what to fix when one refuses. They work with any S3-compatible endpoint (a cloud provider's, or a MinIO, Garage or NAS on your network) and nothing in fyisp is specific to a vendor or a region. Open them in **Details → Storage**. The API is in [STORAGE-API.md](STORAGE-API.md).

## Targets

A target is one endpoint you save:

- **Name**: your label for it.
- **Endpoint**: `https://host` or `https://host:port`, as your provider lists it ("S3 endpoint", "S3 API URL" or "S3-compatible endpoint"). `http://` works only for a server on your own network (an RFC 1918 or loopback address, not carrier-grade NAT), so keys are never signed over plain HTTP across the internet.
- **Region**: as your provider names it (`us-east-1`, `auto`, or the server's own name). It is part of every signature; empty means `us-east-1`.
- **Bucket** (optional): the bucket a check asks for. Without one, a check asks for the endpoint itself.
- **Bucket address**: *in the host name* (`bucket.host`, most clouds) or *in the path* (`host/bucket`, most self-hosted servers, and any endpoint given as an IP address). A bucket name with dots needs *in the path* over HTTPS: the dots break the certificate check otherwise.
- **Uses its own certificate authority**: for a self-hosted server whose certificate your system doesn't trust, paste the certificate authority's PEM. fyisp then trusts that authority for this target only. It never skips the certificate check.

When you save a target, fyisp looks its name up and notes whether it is on your network or on the internet. Later connections must go to an address of the same kind, so a name can't be switched to point somewhere else (DNS rebinding). fyisp never connects to link-local addresses (cloud metadata), multicast, reserved, benchmarking or documentation addresses. IPv6 isn't used.

Targets are kept in `storage-targets.json` in the data directory (`fyisp paths` shows where), readable by fyisp's user only. It is not part of the database: back it up with the database if you want to keep your targets.

## Check

**Check** sends one request to the target: an unsigned `HEAD` of the bucket (or of the endpoint), or, while a campaign holds keys for the target, a listing of `fyisp-test/` signed with them. It shows whether it connected, the time for the name lookup, the connection, encryption and the first byte, and the HTTP status it answered. Any answer shows the endpoint is up: without keys, an "access denied" (403) is what a private bucket says, and a 404 or a redirect can mean the bucket isn't at this endpoint or in this region. When encryption fails, it says what to fix: the bucket address (a bucket name with dots) or the certificate authority. A check moves almost no data and stores only its result, in memory, until fyisp restarts or the target's settings change.

Targets never hold keys. A signed check, with keys sent along with it, is available through the API ([STORAGE-API.md](STORAGE-API.md)); it needs only permission to list the bucket under `fyisp-test/`, so use keys scoped to a test bucket, or short-lived session credentials.

## Campaigns

A campaign tests its targets over time: a round every few minutes of fresh connections (each with its own name lookup and handshake, as an app makes after being idle), requests on one kept connection, and, for targets given keys, a download and an upload. Every attempt is stored, failures included, so you can see when and how often each target fails. Start one with **New campaign…** on the Storage page (or **Run one round now…** for a single round, first in the queue), or through the API ([STORAGE-API.md](STORAGE-API.md#campaigns)):

- **On demand:** now or at a time, for a number of rounds or a while.
- **Recurring:** on weekdays inside a window of your choice (say 7 to 11 pm), in your time zone, until a date.

Rounds wait their turn in the speed-test queue, so they never run during a speed test, and their transfers are marked on the charts like speed tests. A round that can't run (another test held the line too long, the campaign was paused) is stored as skipped, and the time fyisp wasn't running as a gap.

**Keys** are entered when a campaign starts and tried once first (refused keys stop the start). An on-demand campaign keeps them in memory only: after a restart it asks for them again, testing connections meanwhile. A recurring schedule saves them in `storage-targets.json` (readable by fyisp's user only) and deletes them when it ends or you remove it. Changing a target's endpoint, region, bucket, addressing or certificate authority deletes the keys held for it, and **Forget keys…** on its row deletes them at once. Give fyisp keys that may only list, get, put and delete under `fyisp-test/` in a test bucket.

**While it runs** the header says what runs and when the next round is, with **Pause**, **Resume** and **Stop campaign…** (one that hasn't started yet: **Cancel campaign…**; a schedule: **Pause schedule**, **Resume schedule** and **Remove schedule…**). When scheduled speed tests are on, the page offers to pause them while the campaign runs (nothing is paused by itself). After a restart an on-demand campaign shows *Waiting for keys* with **Enter keys…**.

**Results:** the **Rounds** grid has one column per round and one row per target; each cell shows the worst thing in that round (all worked, slow, failed, refused, no keys, didn't run, fyisp not running, cancelled) as a colour, a mark and a sentence for screen readers. Arrow keys move through it, and Enter (or a click) shows that round's attempts: each step's timings, result, speed, the provider's address as a /24 and its request ID. **Failures** lists the failed attempts, newest first, by target; **Past campaigns** shows earlier ones.

**What it points to:** a campaign's **finding** reads its rounds: *too early to tell* (under 5 rounds), *no problems found*, a *configuration problem* (only keys, clock or settings refused), *your line* (your router or your provider's first router lost packets in the same rounds, or every target's connections failed at once), *the provider* (mostly its own server errors or slow-downs), or *the path* (failures on some endpoints, not all at once, with no loss on your line to explain them), with the evidence: failures per target, the most frequent kinds, a provider address range that took most of them. Each target's card has its failure counts, connection, TLS and first-byte times (median and 95th percentile) and transfer speeds. After the first round, and after a connection failure, fyisp traceroutes the provider address (as Investigate does), so a route change shows next to the failures.

**Reports:** *Copy report* gives the campaign as Markdown (the finding, a table per target, your line, the traceroutes, the newest failures; endpoint hosts only if you tick the box, since they can hold account IDs), and *Download CSV* every attempt. `fyisp export --storage` writes the same CSV for every campaign. Metrics: `fyisp_storage_*` on `/metrics`.

**Data:** a round with keys moves its download and upload sizes per target, plus about 1 KiB per request; fyisp deletes every upload at once. The first time a download size is used on a bucket, fyisp also uploads an object of that size (and a 1 KiB one) and keeps it there for later rounds and campaigns: delete `fyisp-test/` in the bucket when you're done. A download size that would take over 5 minutes to upload at your recent speed-test upload speed is refused. The estimate (`part=estimate`) says how much a campaign moves, the seeding left out.

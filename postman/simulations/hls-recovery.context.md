# HLS recovery simulation

Created 2026-10-25 from the response fields consumed in `ckey.go` and the HLS
fixtures in `channels_test.go`. No official upstream contract or published mock
was available locally; the CLI was signed out, so the owner mock search was skipped.

## Purpose

Exercise the real Go HTTP handler and HTTP client through stale-cache refresh,
upstream HTML/503 responses, shared retry backoff, delayed-primary racing,
frozen-primary failover and bandwidth-based variant selection. The only member,
`upstream-hls`, supplies the CDN playlist and bkliveinfo response fields this path
consumes; see its handler for fixture routes.

## Execution

Start in one terminal from the repository root:

```sh
postman simulation run postman/simulations/hls-recovery.sim.yaml --port auto --no-history --output ndjson
```

Take the printed member address from the `listening` event. In another terminal,
set `HLS_SIMULATION_URL` to that address and run:

```sh
HLS_SIMULATION_URL=http://127.0.0.1:60243/upstream go test -race ./cmd/ysp-live -run '^TestPostman' -v
```

Port 60243 was allocated in the 2026-10-25 run; replace it with the newly printed
port. The test starts a real local application HTTP server and stops it on exit.
Inspect `request` events for playlist and discovery traffic, then Ctrl-C the
simulation. Do not commit request logs: generated query strings include cKeys.

## Assumptions

- The fixture models only consumed fields, not signature verification, actual
  token expiry, CDN media bytes, or live upstream availability. It cannot prove
  player compatibility or months of uptime. (reasoned 2026-10-25, mock handler)
- Test routing changes only the destination before sending real HTTP, so no
  production URLs or credentials need changing. (observed 2026-10-25)
- Each fixture source has a fixed media sequence. The frozen-source test ages
  progress state to model a stopped stream without waiting 30 seconds; ordinary
  recovery tests advance retry state, and cooldown expiry also has a timed local
  HTTP regression test. (observed 2026-10-25)
- The test transport preserves the logical upstream response URL when rewriting
  network destinations. Otherwise relative variants acquire the simulation route
  prefix twice and fail with 501. (observed 2026-10-25)

## Observations

<!-- begin:observations -->
2026-10-25: The sole member served discovery, healthy/frozen playlists and master
variants. A delayed-primary test including discovery took 264 ms initially,
4 ms on preferred-source refresh and under 1 ms from cache; frozen-to-healthy
failover took 4 ms. These are local fixture timings, not actual video startup.
The complete race-enabled suite subsequently passed, including the low-bandwidth
selection scenario after correcting test transport origin handling. No member
stayed silent.

2026-10-25: The sole member served playlist and discovery requests. The real
application returned 200, then 503 without stale media, then 200 after recovery;
the test passed under the race detector. No members stayed silent.
<!-- end:observations -->

## What didn't work

- 2026-10-25: CLI 1.73.0 rejected a member folder path with “Mock 'file' must point
  to a file”; the saved simulation points explicitly to `config.yaml` instead.

- 2026-10-25: The first quality scenario returned 501 for a doubled route prefix;
  preserving the logical response URL in the test transport fixed the fixture
  wiring. Subsequent full simulation runs passed.

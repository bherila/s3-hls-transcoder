# TESTING.md

Testing guide for `s3-hsts-transcoder`. See [CLAUDE.md](./CLAUDE.md) for project conventions, [SPEC.md](./SPEC.md) for the behavioral contract being tested.

## Running tests

```bash
pnpm test          # all packages (lib + entrypoints)
pnpm -C lib test   # lib only
```

All tests live in `lib/src/`. The entrypoint packages (`aws`, `cloudflare`, `local`) run `vitest --passWithNoTests` and contain no tests today.

## Test file naming

| Pattern | Description |
|---|---|
| `*.test.ts` | Pure unit tests — no S3 or ffmpeg required |
| `*.s3mock.test.ts` | Unit tests that exercise S3 I/O paths via `aws-sdk-client-mock` |

Separate the suffix so `vitest` can run both by default and so it's obvious at a glance whether a test file touches S3 API paths.

## Mocking strategy

**S3**: Use `aws-sdk-client-mock` (`mockClient(S3Client)`). Reset with `s3Mock.reset()` in `beforeEach`/`afterEach`. Never hit a real bucket in unit tests.

S3 error stubs use `S3ServiceException` directly — construct them with `name`, `$fault`, `$metadata.httpStatusCode`. The two that appear most often are `PreconditionFailed` (412, conditional PUT conflict) and `NoSuchKey` (404).

**ffmpeg**: Not mocked in the current test suite. The `fingerprint.test.ts` tests the hash math and serialization in isolation without invoking the binary. The `ffmpeg/` source files (`transcode.ts`, `signature.ts`, `probe.ts`) have no automated tests yet — these require a real ffmpeg binary.

**Logger**: Pass a `silentLogger` (object with no-op `debug`/`info`/`warn`/`error` methods) to any function that takes a `Logger`.

## What is and isn't covered

| Area | Covered | Notes |
|---|---|---|
| Lock acquire / stale takeover / release | Yes | `lock.s3mock.test.ts` |
| Mapping read / write / dedup check | Yes | `mapping.s3mock.test.ts` |
| Content ID (SHA-256 scheme prefix) | Yes | `contentId.test.ts` |
| Fingerprint math + serialization | Yes | `fingerprint.test.ts` |
| Config parsing + overlap validation | Yes | `config.test.ts` |
| Scanner (S3 list) | Yes | `scanner.test.ts` |
| Uploader | Yes | `uploader.test.ts` |
| ffmpeg transcode / probe / signature | **No** | Requires real binary |
| Full orchestrator pipeline | **No** | Integration test — see below |
| Real S3 / R2 / MinIO round-trip | **No** | Integration test — see below |

## Integration tests

Not yet implemented. A GitHub issue template exists for the MinIO integration test. When added, integration tests should:
- Live in a separate package or directory (not mixed with unit tests)
- Require a running MinIO instance (Docker)
- Gate on an env var (e.g. `INTEGRATION=1`) so they don't run in standard `pnpm test`

## Adding new tests

- S3 I/O paths → `*.s3mock.test.ts`, use `mockClient`
- Pure logic → `*.test.ts`
- New ffmpeg wrapper → mark as requiring real binary in the test file and skip in CI until the integration harness exists
- Always test the error paths, not just the happy path — the lock/lease design assumes retries are safe, so test the race conditions (412 → stale check → re-PUT, etc.)

## CI

The CI job (`ci.yml`) runs:
1. `pnpm --filter @s3-hsts-transcoder/lib build` — builds `lib/dist` so downstream `tsc --noEmit` can resolve types
2. `pnpm typecheck` — all packages
3. `pnpm build` — all packages
4. `pnpm test` — all packages

ffmpeg is not installed in the CI runner. Any test that requires the binary must be skipped or placed behind an integration gate.

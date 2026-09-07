# TESTING.md

Testing guide for `s3-hls-transcoder`. See [CLAUDE.md](./CLAUDE.md) for project conventions, [SPEC.md](./SPEC.md) for the behavioral contract being tested.

## Running tests

```bash
pnpm test          # all packages (lib + entrypoints)
pnpm -C lib test   # lib only
```

All tests live in `lib/src/`. The entrypoint packages (`aws`, `cloudflare`, `local`) run `vitest --passWithNoTests` and contain no tests today.

## Test file naming

| Pattern            | Description                                                     |
| ------------------ | --------------------------------------------------------------- |
| `*.test.ts`        | Pure unit tests — no S3 or ffmpeg required                      |
| `*.s3mock.test.ts` | Unit tests that exercise S3 I/O paths via `aws-sdk-client-mock` |

Separate the suffix so `vitest` can run both by default and so it's obvious at a glance whether a test file touches S3 API paths.

## Mocking strategy

**S3**: Use `aws-sdk-client-mock` (`mockClient(S3Client)`). Reset with `s3Mock.reset()` in `beforeEach`/`afterEach`. Never hit a real bucket in unit tests.

S3 error stubs use `S3ServiceException` directly — construct them with `name`, `$fault`, `$metadata.httpStatusCode`. The two that appear most often are `PreconditionFailed` (412, conditional PUT conflict) and `NoSuchKey` (404).

**ffmpeg**: Not mocked in the current test suite. The `fingerprint.test.ts` tests the hash math and serialization in isolation without invoking the binary. The `ffmpeg/` source files (`transcode.ts`, `signature.ts`, `probe.ts`) have no automated tests yet — these require a real ffmpeg binary.

**Logger**: Pass a `silentLogger` (object with no-op `debug`/`info`/`warn`/`error` methods) to any function that takes a `Logger`.

## What is and isn't covered

| Area                                    | Covered | Notes                                              |
| --------------------------------------- | ------- | -------------------------------------------------- |
| Lock acquire / stale takeover / release | Yes     | `lock.s3mock.test.ts`                              |
| Mapping read / write / dedup check      | Yes     | `mapping.s3mock.test.ts`                           |
| Content ID (SHA-256 scheme prefix)      | Yes     | `contentId.test.ts`                                |
| Fingerprint math + serialization        | Yes     | `fingerprint.test.ts`                              |
| Config parsing + overlap validation     | Yes     | `config.test.ts`                                   |
| Scanner (S3 list)                       | Yes     | `scanner.test.ts`                                  |
| Uploader                                | Yes     | `uploader.test.ts`                                 |
| ffmpeg transcode / probe / signature    | **No**  | Requires real binary; covered by integration tests |
| Full orchestrator pipeline              | **Yes** | `integration/` — see below                         |
| Real S3 / R2 / MinIO round-trip         | **Yes** | `integration/` — see below                         |

## Integration tests

Live in `integration/`. Testcontainers starts a real MinIO; `ffmpeg` generates the fixtures; the full `runOnce()` pipeline runs against them and the assertions read the destination bucket back. Three suites:

| Suite                         | Covers                                                                                   |
| ----------------------------- | ---------------------------------------------------------------------------------------- |
| `runOnce.integration.test.ts` | Output layout, mapping contract, lock release, ladder filtering, metadata, cached re-run |
| `dedup.integration.test.ts`   | Byte-hash dedup, and that a perceptual match stays advisory                              |
| `cleanup.integration.test.ts` | Refcounted GC: retention, orphan-mapping deletion, dry run, index cleanup                |

See [integration/README.md](./integration/README.md) for the fixtures and the assertion list.

**Requirements**: Docker daemon running, `ffmpeg` on PATH (or `FFMPEG_PATH` set).

```sh
INTEGRATION=1 pnpm --filter @s3-hls-transcoder/integration test
```

Not run by `pnpm test` — without `INTEGRATION=1` the suites skip, so the standard recursive run stays usable on a machine without Docker.

## Adding new tests

- S3 I/O paths → `*.s3mock.test.ts`, use `mockClient`
- Pure logic → `*.test.ts`
- New ffmpeg wrapper → mark as requiring real binary in the test file and skip in CI until the integration harness exists
- Always test the error paths, not just the happy path — the lock/lease design assumes retries are safe, so test the race conditions (412 → stale check → re-PUT, etc.)

## CI

`ci.yml` runs three jobs on every pull request:

| Job           | Runs                                                                                                  |
| ------------- | ----------------------------------------------------------------------------------------------------- |
| `build`       | `gofmt`, `go vet`, `go build`, `go test ./... -race`                                                  |
| `node`        | `pnpm format:check`, `pnpm lint`, `pnpm build`, `pnpm typecheck`, `pnpm test` (unit suites)           |
| `integration` | Installs ffmpeg, builds `lib`, then `INTEGRATION=1 pnpm --filter @s3-hls-transcoder/integration test` |

`node` builds before typechecking because the entrypoint and integration packages resolve `@s3-hls-transcoder/lib` through `lib/dist`.

The `node` job has no ffmpeg: a unit test that needs the binary must be skipped or moved behind the integration gate. The `integration` job installs it, and gets MinIO from the runner's Docker daemon via Testcontainers.

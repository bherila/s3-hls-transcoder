# lib — shared transcoding logic

All real work lives here. Three entrypoint packages (`aws/`, `cloudflare/`, `local/`) consume this lib via `workspace:*`.

See **[../PLAN.md](../PLAN.md)** for architecture and the planned API surface, and **[../CLAUDE.md](../CLAUDE.md)** for conventions.

## Develop

```sh
pnpm build       # tsc to dist/
pnpm test        # vitest
pnpm typecheck
```

## Public API

Empty in initial scaffolding. Planned modules: scanner, hasher, fingerprinter, transcoder, mapping I/O, lock, lease, ladder. See [PLAN.md](../PLAN.md).

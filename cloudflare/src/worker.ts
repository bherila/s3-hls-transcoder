// @ts-nocheck
//
// Cloudflare Workers shim that fronts the container in `./index.ts`.
//
// THIS IS A SKELETON. The Cloudflare Containers + Durable Objects + Worker
// binding surface has been evolving; verify against current docs before
// deploying:
//   https://developers.cloudflare.com/containers/
//
// Excluded from the package's main `tsc` build (see ../tsconfig.json) — the
// container build only needs `index.ts`. This file runs in the V8 isolate
// (no Node APIs); install `@cloudflare/workers-types` and remove the
// `@ts-nocheck` once you're ready to type-check it locally.
//
// Roles:
//   - default export: cron handler. Wakes the DO singleton on each tick.
//   - TranscoderContainer (DurableObject): owns a single Container instance
//     and routes `/run` requests into it. The Container's HTTP server (or
//     just `node dist/index.js` running runOnce) is what does the work.

export class TranscoderContainer {
  state: DurableObjectState;
  env: Env;
  container: Container;

  constructor(state: DurableObjectState, env: Env) {
    this.state = state;
    this.env = env;
    // Binding name `this.ctx.container` / `state.container` has shifted
    // between Containers preview revisions — confirm in current docs.
    this.container = state.container as Container;
  }

  async fetch(req: Request): Promise<Response> {
    if (!this.container.running) {
      this.container.start();
    }
    // Forward the request into the container so its entrypoint can handle
    // it, OR just return immediately and rely on the container's CMD to run
    // `runOnce()` and exit. The latter is what `Dockerfile` is set up for.
    return new Response("started", { status: 202 });
  }
}

export default {
  async scheduled(_event: ScheduledEvent, env: Env, ctx: ExecutionContext): Promise<void> {
    const id = env.TRANSCODER_DO.idFromName("singleton");
    const stub = env.TRANSCODER_DO.get(id);
    ctx.waitUntil(stub.fetch("https://do/run"));
  },
};

interface Env {
  TRANSCODER_DO: DurableObjectNamespace;
}

import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    // Integration tests need Docker + ffmpeg; they only run when INTEGRATION=1.
    // The globalSetup hook enforces this gate so the suite is skipped (not just
    // omitted from the run) when the env var is absent.
    include: ["src/**/*.test.ts"],
    globalSetup: ["src/globalSetup.ts"],
    // Each test can take several minutes (transcode + upload).
    testTimeout: 300_000,
    // Setup hooks pull the MinIO image, create buckets, and transcode fixtures.
    hookTimeout: 300_000,
    // Run serially – each test brings up its own container anyway, and
    // parallel containers multiply Docker resource usage for no benefit.
    pool: "forks",
    poolOptions: {
      forks: {
        singleFork: true,
      },
    },
    reporters: ["verbose"],
  },
});

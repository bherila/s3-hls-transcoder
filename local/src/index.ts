import "dotenv/config";
import {
  createLogger,
  createS3Client,
  loadConfig,
  runOnce,
  VERSION,
} from "@s3-hsts-transcoder/lib";

async function main(): Promise<void> {
  const config = loadConfig("local");
  const logger = createLogger(config.logLevel);
  logger.info("transcoder starting", { platform: "local", lib: VERSION });

  const sourceClient = createS3Client(config.source);
  const destClient = createS3Client(config.dest);

  const summary = await runOnce({ config, sourceClient, destClient, logger });
  if (summary.failed > 0) process.exitCode = 1;
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});

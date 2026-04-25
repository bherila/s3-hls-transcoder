import {
  createLogger,
  createS3Client,
  loadConfig,
  runOnce,
  VERSION,
} from "@s3-hsts-transcoder/lib";

export const handler = async (): Promise<{ statusCode: number; body: string }> => {
  const config = loadConfig("aws-lambda");
  const logger = createLogger(config.logLevel);
  logger.info("transcoder starting", { platform: "aws-lambda", lib: VERSION });

  const sourceClient = createS3Client(config.source);
  const destClient = createS3Client(config.dest);

  const summary = await runOnce({ config, sourceClient, destClient, logger });
  return {
    statusCode: summary.failed > 0 ? 500 : 200,
    body: JSON.stringify(summary),
  };
};

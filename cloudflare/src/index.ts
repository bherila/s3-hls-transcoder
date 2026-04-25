import { VERSION } from "@s3-hsts-transcoder/lib";

async function main(): Promise<void> {
  console.log(`s3-hsts-transcoder cloudflare container starting (lib v${VERSION})`);
  // TODO: implement transcoding pipeline (see PLAN.md).
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});

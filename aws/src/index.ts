import { VERSION } from "@s3-hsts-transcoder/lib";

export const handler = async (): Promise<{ statusCode: number; body: string }> => {
  console.log(`s3-hsts-transcoder aws lambda starting (lib v${VERSION})`);
  // TODO: implement transcoding pipeline (see PLAN.md).
  return { statusCode: 200, body: "ok" };
};

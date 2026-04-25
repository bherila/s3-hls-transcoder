import { HeadObjectCommand, type S3Client } from "@aws-sdk/client-s3";
import { masterPlaylistKey } from "./contentId.js";
import { isNotFound } from "./s3.js";

/**
 * Has this content already been transcoded? Checks for the master playlist
 * at the canonical `by-id/<contentId>/master.m3u8` location.
 */
export async function transcodedOutputExists(
  client: S3Client,
  bucket: string,
  contentId: string,
): Promise<boolean> {
  try {
    await client.send(
      new HeadObjectCommand({ Bucket: bucket, Key: masterPlaylistKey(contentId) }),
    );
    return true;
  } catch (err) {
    if (isNotFound(err)) return false;
    throw err;
  }
}

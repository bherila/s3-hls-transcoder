import {
  DeleteObjectsCommand,
  HeadObjectCommand,
  ListObjectsV2Command,
  type S3Client,
} from "@aws-sdk/client-s3";
import { byIdPrefix, masterPlaylistKey } from "./contentId.js";
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
    await client.send(new HeadObjectCommand({ Bucket: bucket, Key: masterPlaylistKey(contentId) }));
    return true;
  } catch (err) {
    if (isNotFound(err)) return false;
    throw err;
  }
}

/**
 * Removes everything under `by-id/<contentId>/`. Used by refcount-aware
 * cleanup when no mappings still reference the content ID. Returns the number
 * of objects deleted.
 */
export async function deleteByIdDirectory(
  client: S3Client,
  bucket: string,
  contentId: string,
): Promise<number> {
  const prefix = byIdPrefix(contentId);
  let deleted = 0;
  let continuationToken: string | undefined;

  do {
    const res = await client.send(
      new ListObjectsV2Command({
        Bucket: bucket,
        Prefix: prefix,
        ContinuationToken: continuationToken,
      }),
    );

    const objects = (res.Contents ?? []).filter((o) => o.Key);
    if (objects.length > 0) {
      // S3 DeleteObjects caps at 1000 keys per call.
      for (let i = 0; i < objects.length; i += 1000) {
        const batch = objects.slice(i, i + 1000);
        await client.send(
          new DeleteObjectsCommand({
            Bucket: bucket,
            Delete: {
              Objects: batch.map((o) => ({ Key: o.Key! })),
            },
          }),
        );
        deleted += batch.length;
      }
    }

    continuationToken = res.NextContinuationToken;
  } while (continuationToken);

  return deleted;
}

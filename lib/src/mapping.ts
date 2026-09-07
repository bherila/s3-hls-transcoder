import {
  GetObjectCommand,
  ListObjectsV2Command,
  PutObjectCommand,
  type S3Client,
} from "@aws-sdk/client-s3";
import { isNotFound } from "./s3.js";

const MAPPING_PREFIX = "mappings/";
const MAPPING_SUFFIX = ".json";

export interface SourceMapping {
  sourceKey: string;
  sourceBucket: string;
  sourceEndpoint: string;
  sourceEtag: string;
  sourceSize: number;
  sourceLastModified: string;
  contentId: string;
  hlsRoot: string;
  encodedAt: string;
  encoderVersion: string;
}

export function mappingKey(sourceKey: string): string {
  return `${MAPPING_PREFIX}${sourceKey}${MAPPING_SUFFIX}`;
}

function sourceKeyFromMappingKey(key: string): string | null {
  if (!key.startsWith(MAPPING_PREFIX) || !key.endsWith(MAPPING_SUFFIX)) return null;
  return key.slice(MAPPING_PREFIX.length, key.length - MAPPING_SUFFIX.length);
}

export async function readMapping(
  client: S3Client,
  bucket: string,
  sourceKey: string,
): Promise<SourceMapping | null> {
  try {
    const res = await client.send(
      new GetObjectCommand({ Bucket: bucket, Key: mappingKey(sourceKey) }),
    );
    if (!res.Body) throw new Error(`Empty body for ${mappingKey(sourceKey)}`);
    const text = await res.Body.transformToString();
    return JSON.parse(text) as SourceMapping;
  } catch (err) {
    if (isNotFound(err)) return null;
    throw err;
  }
}

export async function writeMapping(
  client: S3Client,
  bucket: string,
  mapping: SourceMapping,
): Promise<void> {
  await client.send(
    new PutObjectCommand({
      Bucket: bucket,
      Key: mappingKey(mapping.sourceKey),
      Body: JSON.stringify(mapping, null, 2),
      ContentType: "application/json",
    }),
  );
}

/**
 * Cheap "have we processed this exact source before?" check.
 * Source ETag + size match → skip. ETag is bucket-specific (multipart
 * uploads produce different ETags across tools), but stable for the *same*
 * upload, which is what we need here.
 */
export function isCachedMapping(
  mapping: SourceMapping | null,
  current: { etag: string; size: number },
): boolean {
  if (!mapping) return false;
  return mapping.sourceEtag === current.etag && mapping.sourceSize === current.size;
}

/**
 * Returns source keys whose mapping currently points at `contentId`. Used by
 * cleanup to determine whether a content ID is still referenced.
 *
 * O(N) GETs over the mappings/ prefix — the fallback used to backfill the
 * by-id/<id>/refs.json reverse index (see `listRefs`), not a hot path.
 */
export async function findMappingsForContentId(
  client: S3Client,
  bucket: string,
  contentId: string,
): Promise<string[]> {
  const result: string[] = [];
  let continuationToken: string | undefined;

  do {
    const res = await client.send(
      new ListObjectsV2Command({
        Bucket: bucket,
        Prefix: MAPPING_PREFIX,
        ContinuationToken: continuationToken,
      }),
    );

    for (const obj of res.Contents ?? []) {
      if (!obj.Key) continue;
      const sourceKey = sourceKeyFromMappingKey(obj.Key);
      if (!sourceKey) continue;
      const mapping = await readMapping(client, bucket, sourceKey);
      if (mapping?.contentId === contentId) {
        result.push(sourceKey);
      }
    }

    continuationToken = res.NextContinuationToken;
  } while (continuationToken);

  return result;
}

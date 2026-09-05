import {
  DeleteObjectCommand,
  DeleteObjectsCommand,
  GetObjectCommand,
  ListObjectsV2Command,
  PutObjectCommand,
  S3Client,
  S3ServiceException,
  type DeleteObjectCommandInput,
  type DeleteObjectsCommandInput,
  type GetObjectCommandInput,
  type ListObjectsV2CommandInput,
  type PutObjectCommandInput,
} from "@aws-sdk/client-s3";
import { mockClient } from "aws-sdk-client-mock";
import { Readable } from "node:stream";
import { sdkStreamMixin } from "@smithy/util-stream";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { runCleanupPass } from "./cleanup.js";
import { masterPlaylistKey } from "./contentId.js";
import { fingerprintKey } from "./fingerprintIndex.js";
import { mappingKey, type SourceMapping } from "./mapping.js";
import { refsKey, type ContentRefs } from "./refs.js";

const s3Mock = mockClient(S3Client);

const silentLogger = {
  debug: () => {},
  info: () => {},
  warn: () => {},
  error: () => {},
};

const SOURCE_BUCKET = "source-a";
const SOURCE_ENDPOINT = "https://source-a.example.com";
const DEST_BUCKET = "dest";

/**
 * Minimal in-memory stand-in for the calls the cleanup pass makes. Object
 * bodies are keyed by "<bucket>/<key>"; reads and deletes are recorded so tests
 * can assert on I/O volume, not just the end state.
 */
class FakeS3 {
  readonly objects = new Map<string, string>();
  readonly gets: string[] = [];
  readonly deletes: string[] = [];

  put(bucket: string, key: string, value: unknown): void {
    this.objects.set(`${bucket}/${key}`, JSON.stringify(value));
  }

  has(bucket: string, key: string): boolean {
    return this.objects.has(`${bucket}/${key}`);
  }

  json<T>(bucket: string, key: string): T {
    const body = this.objects.get(`${bucket}/${key}`);
    if (body === undefined) throw new Error(`missing object ${bucket}/${key}`);
    return JSON.parse(body) as T;
  }

  getCount(bucket: string, key: string): number {
    return this.gets.filter((g) => g === `${bucket}/${key}`).length;
  }

  install(): void {
    s3Mock.on(ListObjectsV2Command).callsFake((input: ListObjectsV2CommandInput) => {
      const prefix = input.Prefix ?? "";
      const contents = [...this.objects.keys()]
        .filter((k) => k.startsWith(`${input.Bucket}/`))
        .map((k) => k.slice(`${input.Bucket}/`.length))
        .filter((k) => k.startsWith(prefix))
        .sort()
        .map((k) => ({ Key: k, ETag: `"etag-${k}"`, Size: 1024, LastModified: new Date(0) }));
      return { Contents: contents };
    });
    s3Mock.on(GetObjectCommand).callsFake((input: GetObjectCommandInput) => {
      const id = `${input.Bucket}/${input.Key}`;
      this.gets.push(id);
      const body = this.objects.get(id);
      if (body === undefined) throw notFound();
      return { Body: sdkStreamMixin(Readable.from([Buffer.from(body)])) };
    });
    s3Mock.on(PutObjectCommand).callsFake((input: PutObjectCommandInput) => {
      this.objects.set(`${input.Bucket}/${input.Key}`, String(input.Body));
      return {};
    });
    s3Mock.on(DeleteObjectCommand).callsFake((input: DeleteObjectCommandInput) => {
      const id = `${input.Bucket}/${input.Key}`;
      this.objects.delete(id);
      this.deletes.push(id);
      return {};
    });
    s3Mock.on(DeleteObjectsCommand).callsFake((input: DeleteObjectsCommandInput) => {
      for (const obj of input.Delete?.Objects ?? []) {
        const id = `${input.Bucket}/${obj.Key}`;
        this.objects.delete(id);
        this.deletes.push(id);
      }
      return {};
    });
  }
}

function notFound(): S3ServiceException {
  return new S3ServiceException({
    name: "NoSuchKey",
    $fault: "client",
    $metadata: { httpStatusCode: 404 },
    message: "Not Found",
  });
}

function mapping(overrides: Partial<SourceMapping> = {}): SourceMapping {
  return {
    sourceKey: "b/live-in-pair-B.mp4",
    sourceBucket: "source-b",
    sourceEndpoint: "https://source-b.example.com",
    sourceEtag: "etag",
    sourceSize: 1024,
    sourceLastModified: "2026-01-01T00:00:00Z",
    contentId: "sha256:pair-b-content",
    hlsRoot: "by-id/sha256:pair-b-content/master.m3u8",
    encodedAt: "2026-01-01T00:00:00Z",
    encoderVersion: "0.1.0",
    ...overrides,
  };
}

function ownedMapping(sourceKey: string, contentId: string): SourceMapping {
  return mapping({
    sourceKey,
    sourceBucket: SOURCE_BUCKET,
    sourceEndpoint: SOURCE_ENDPOINT,
    contentId,
    hlsRoot: masterPlaylistKey(contentId),
  });
}

function refs(contentId: string, sourceKeys: string[]): ContentRefs {
  return { version: 1, contentId, sourceKeys, updatedAt: "2026-01-01T00:00:00Z" };
}

function cleanup(fake: FakeS3, dryRun = false) {
  return runCleanupPass({
    sourceClient: new S3Client({}),
    destClient: new S3Client({}),
    sourceBucket: SOURCE_BUCKET,
    sourceEndpoint: SOURCE_ENDPOINT,
    destBucket: DEST_BUCKET,
    logger: silentLogger,
    dryRun,
  });
}

let fake: FakeS3;

beforeEach(() => {
  s3Mock.reset();
  fake = new FakeS3();
  fake.install();
});
afterEach(() => s3Mock.reset());

describe("runCleanupPass", () => {
  it("does not delete mappings owned by another source bucket in a shared destination", async () => {
    fake.put(SOURCE_BUCKET, "a/live.mp4", { data: "x" });
    fake.put(DEST_BUCKET, mappingKey("b/live-in-pair-B.mp4"), mapping());

    const result = await cleanup(fake);

    expect(result).toEqual({
      orphanMappingsFound: 0,
      orphanMappingsDeleted: 0,
      contentIdsGcd: 0,
      contentIdsRetained: 0,
      objectsDeleted: 0,
    });
    expect(fake.deletes).toHaveLength(0);
  });

  it("deletes orphan mappings owned by the current source bucket", async () => {
    const contentId = "sha256:pair-a-content";
    fake.put(DEST_BUCKET, mappingKey("a/deleted.mp4"), ownedMapping("a/deleted.mp4", contentId));
    fake.put(DEST_BUCKET, masterPlaylistKey(contentId), { m3u8: "x" });
    fake.put(DEST_BUCKET, fingerprintKey(contentId), { fp: "x" });

    const result = await cleanup(fake);

    expect(result.orphanMappingsFound).toBe(1);
    expect(result.orphanMappingsDeleted).toBe(1);
    expect(result.contentIdsGcd).toBe(1);
    expect(fake.has(DEST_BUCKET, mappingKey("a/deleted.mp4"))).toBe(false);
    expect(fake.has(DEST_BUCKET, masterPlaylistKey(contentId))).toBe(false);
    expect(fake.has(DEST_BUCKET, fingerprintKey(contentId))).toBe(false);
  });

  it("uses the reverse index instead of scanning every mapping", async () => {
    const contentId = "sha256:orphan-content";
    fake.put(DEST_BUCKET, mappingKey("a/deleted.mp4"), ownedMapping("a/deleted.mp4", contentId));
    fake.put(DEST_BUCKET, refsKey(contentId), refs(contentId, ["a/deleted.mp4"]));
    fake.put(DEST_BUCKET, masterPlaylistKey(contentId), { m3u8: "x" });

    // Unrelated live content that a whole-mappings scan would have read.
    const untouched: string[] = [];
    for (let i = 0; i < 20; i++) {
      const key = `a/live-${String(i).padStart(2, "0")}.mp4`;
      fake.put(SOURCE_BUCKET, key, { data: "x" });
      fake.put(DEST_BUCKET, mappingKey(key), ownedMapping(key, `sha256:live-${i}`));
      untouched.push(mappingKey(key));
    }

    const result = await cleanup(fake);

    expect(result.contentIdsGcd).toBe(1);
    expect(result.orphanMappingsDeleted).toBe(1);
    for (const key of untouched) {
      expect(fake.getCount(DEST_BUCKET, key)).toBe(0);
    }
  });

  it("backfills the reverse index for content written before it existed", async () => {
    const contentId = "sha256:legacy-content";
    fake.put(SOURCE_BUCKET, "a/live.mp4", { data: "x" });
    fake.put(DEST_BUCKET, mappingKey("a/live.mp4"), ownedMapping("a/live.mp4", contentId));
    fake.put(DEST_BUCKET, mappingKey("a/deleted.mp4"), ownedMapping("a/deleted.mp4", contentId));
    fake.put(DEST_BUCKET, masterPlaylistKey(contentId), { m3u8: "x" });

    const result = await cleanup(fake);

    expect(result.contentIdsRetained).toBe(1);
    expect(result.contentIdsGcd).toBe(0);
    expect(fake.has(DEST_BUCKET, masterPlaylistKey(contentId))).toBe(true);
    // Backfilled from the scan, then pruned down to the surviving reference.
    expect(fake.json<ContentRefs>(DEST_BUCKET, refsKey(contentId)).sourceKeys).toEqual([
      "a/live.mp4",
    ]);
  });

  it("retains content with a live reference and prunes the orphan from the index", async () => {
    const contentId = "sha256:shared-content";
    fake.put(SOURCE_BUCKET, "a/live.mp4", { data: "x" });
    fake.put(DEST_BUCKET, mappingKey("a/live.mp4"), ownedMapping("a/live.mp4", contentId));
    fake.put(DEST_BUCKET, mappingKey("a/deleted.mp4"), ownedMapping("a/deleted.mp4", contentId));
    fake.put(DEST_BUCKET, refsKey(contentId), refs(contentId, ["a/deleted.mp4", "a/live.mp4"]));
    fake.put(DEST_BUCKET, masterPlaylistKey(contentId), { m3u8: "x" });

    const result = await cleanup(fake);

    expect(result.contentIdsRetained).toBe(1);
    expect(fake.has(DEST_BUCKET, masterPlaylistKey(contentId))).toBe(true);
    expect(fake.json<ContentRefs>(DEST_BUCKET, refsKey(contentId)).sourceKeys).toEqual([
      "a/live.mp4",
    ]);
  });

  it("ignores reverse-index entries whose mapping is gone", async () => {
    const contentId = "sha256:stale-ref-content";
    fake.put(DEST_BUCKET, mappingKey("a/deleted.mp4"), ownedMapping("a/deleted.mp4", contentId));
    // "a/vanished.mp4" has no mapping object at all: a reference left behind by
    // an interrupted run. It must not pin the content forever.
    fake.put(DEST_BUCKET, refsKey(contentId), refs(contentId, ["a/deleted.mp4", "a/vanished.mp4"]));
    fake.put(DEST_BUCKET, masterPlaylistKey(contentId), { m3u8: "x" });

    const result = await cleanup(fake);

    expect(result.contentIdsGcd).toBe(1);
    expect(fake.has(DEST_BUCKET, masterPlaylistKey(contentId))).toBe(false);
  });

  it("keeps content alive for another pair's mapping in a shared destination", async () => {
    const contentId = "sha256:cross-pair-content";
    fake.put(DEST_BUCKET, mappingKey("a/deleted.mp4"), ownedMapping("a/deleted.mp4", contentId));
    fake.put(
      DEST_BUCKET,
      mappingKey("b/live.mp4"),
      mapping({ sourceKey: "b/live.mp4", contentId }),
    );
    fake.put(DEST_BUCKET, refsKey(contentId), refs(contentId, ["a/deleted.mp4", "b/live.mp4"]));
    fake.put(DEST_BUCKET, masterPlaylistKey(contentId), { m3u8: "x" });

    const result = await cleanup(fake);

    expect(result.contentIdsRetained).toBe(1);
    expect(fake.has(DEST_BUCKET, masterPlaylistKey(contentId))).toBe(true);
    expect(fake.has(DEST_BUCKET, mappingKey("b/live.mp4"))).toBe(true);
  });

  it("writes nothing in dry-run mode", async () => {
    const contentId = "sha256:dry-run-content";
    fake.put(DEST_BUCKET, mappingKey("a/deleted.mp4"), ownedMapping("a/deleted.mp4", contentId));
    fake.put(DEST_BUCKET, masterPlaylistKey(contentId), { m3u8: "x" });

    const result = await cleanup(fake, true);

    expect(result.orphanMappingsFound).toBe(1);
    expect(result.contentIdsGcd).toBe(1);
    expect(fake.deletes).toHaveLength(0);
    expect(fake.has(DEST_BUCKET, mappingKey("a/deleted.mp4"))).toBe(true);
    expect(fake.has(DEST_BUCKET, refsKey(contentId))).toBe(false);
  });
});

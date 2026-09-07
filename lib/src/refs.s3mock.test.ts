import {
  GetObjectCommand,
  ListObjectsV2Command,
  PutObjectCommand,
  S3Client,
  S3ServiceException,
  type GetObjectCommandInput,
  type ListObjectsV2CommandInput,
  type PutObjectCommandInput,
} from "@aws-sdk/client-s3";
import { mockClient } from "aws-sdk-client-mock";
import { Readable } from "node:stream";
import { sdkStreamMixin } from "@smithy/util-stream";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { mappingKey, type SourceMapping } from "./mapping.js";
import { addRef, listRefs, readRefs, refsKey, removeRef, type ContentRefs } from "./refs.js";

const s3Mock = mockClient(S3Client);
const BUCKET = "dest";
const CONTENT_ID = "sha256:refs-content";

/** Objects the fake serves, keyed by object key (single bucket). */
let objects: Map<string, string>;
let gets: string[];

function installFake(): void {
  s3Mock.on(ListObjectsV2Command).callsFake((input: ListObjectsV2CommandInput) => ({
    Contents: [...objects.keys()]
      .filter((k) => k.startsWith(input.Prefix ?? ""))
      .sort()
      .map((k) => ({ Key: k, ETag: `"etag-${k}"`, Size: 1024, LastModified: new Date(0) })),
  }));
  s3Mock.on(GetObjectCommand).callsFake((input: GetObjectCommandInput) => {
    const key = input.Key!;
    gets.push(key);
    const body = objects.get(key);
    if (body === undefined) {
      throw new S3ServiceException({
        name: "NoSuchKey",
        $fault: "client",
        $metadata: { httpStatusCode: 404 },
        message: "Not Found",
      });
    }
    return { Body: sdkStreamMixin(Readable.from([Buffer.from(body)])) };
  });
  s3Mock.on(PutObjectCommand).callsFake((input: PutObjectCommandInput) => {
    objects.set(input.Key!, String(input.Body));
    return {};
  });
}

function putMapping(sourceKey: string, contentId: string): void {
  const mapping: SourceMapping = {
    sourceKey,
    sourceBucket: "source",
    sourceEndpoint: "https://source.example.com",
    sourceEtag: "etag",
    sourceSize: 1024,
    sourceLastModified: "2026-01-01T00:00:00Z",
    contentId,
    hlsRoot: `by-id/${contentId}/master.m3u8`,
    encodedAt: "2026-01-01T00:00:00Z",
    encoderVersion: "0.1.0",
  };
  objects.set(mappingKey(sourceKey), JSON.stringify(mapping));
}

const client = () => new S3Client({});

beforeEach(() => {
  s3Mock.reset();
  objects = new Map();
  gets = [];
  installFake();
});
afterEach(() => s3Mock.reset());

describe("addRef", () => {
  it("creates the index, appends to it, and never duplicates a key", async () => {
    await addRef(client(), BUCKET, CONTENT_ID, "a/one.mp4", false);
    await addRef(client(), BUCKET, CONTENT_ID, "a/two.mp4", false);
    await addRef(client(), BUCKET, CONTENT_ID, "a/one.mp4", false);

    const refs = await readRefs(client(), BUCKET, CONTENT_ID);
    expect(refs).not.toBeNull();
    expect(refs!.version).toBe(1);
    expect(refs!.contentId).toBe(CONTENT_ID);
    expect(refs!.sourceKeys).toEqual(["a/one.mp4", "a/two.mp4"]);
  });

  // Content transcoded in this pass cannot be referenced by any mapping yet, so
  // creating its index must not scan — that would put the O(N) read this index
  // exists to avoid onto the ingest path of every new upload.
  it("does not scan for freshly transcoded content", async () => {
    putMapping("a/unrelated.mp4", "sha256:other");
    gets = [];

    await addRef(client(), BUCKET, CONTENT_ID, "a/new.mp4", false);

    expect(gets).not.toContain(mappingKey("a/unrelated.mp4"));
    const refs = await readRefs(client(), BUCKET, CONTENT_ID);
    expect(refs!.sourceKeys).toEqual(["a/new.mp4"]);
  });

  // Creating the index from only the incoming key would hide mappings written
  // before the index existed, and cleanup would then GC content they still use.
  it("seeds a new index from existing mappings", async () => {
    putMapping("a/one.mp4", CONTENT_ID);
    putMapping("b/two.mp4", CONTENT_ID);

    await addRef(client(), BUCKET, CONTENT_ID, "a/one.mp4", true);

    const refs = await readRefs(client(), BUCKET, CONTENT_ID);
    expect(refs!.sourceKeys).toEqual(["a/one.mp4", "b/two.mp4"]);
  });
});

describe("refsKey", () => {
  // by-id/ is served to players; the index names source keys belonging to every
  // source that deduped onto the content.
  it("is outside the player-facing by-id/ prefix", () => {
    expect(refsKey(CONTENT_ID).startsWith("by-id/")).toBe(false);
  });
});

describe("readRefs", () => {
  it("returns null when the content has no index", async () => {
    expect(await readRefs(client(), BUCKET, CONTENT_ID)).toBeNull();
  });

  it("rejects an index written by a future version", async () => {
    objects.set(refsKey(CONTENT_ID), JSON.stringify({ version: 2, sourceKeys: [] }));
    await expect(readRefs(client(), BUCKET, CONTENT_ID)).rejects.toThrow(
      /Unsupported refs version/,
    );
  });
});

describe("removeRef", () => {
  it("is a no-op when the index is absent", async () => {
    await removeRef(client(), BUCKET, CONTENT_ID, "a/one.mp4");
    expect(objects.has(refsKey(CONTENT_ID))).toBe(false);
  });

  it("drops only the named key", async () => {
    await addRef(client(), BUCKET, CONTENT_ID, "a/one.mp4", false);
    await addRef(client(), BUCKET, CONTENT_ID, "a/two.mp4", false);

    await removeRef(client(), BUCKET, CONTENT_ID, "a/one.mp4");

    const refs = await readRefs(client(), BUCKET, CONTENT_ID);
    expect(refs!.sourceKeys).toEqual(["a/two.mp4"]);
  });
});

describe("listRefs", () => {
  it("backfills from a mappings scan when no index exists", async () => {
    putMapping("a/legacy.mp4", CONTENT_ID);
    putMapping("a/other.mp4", "sha256:unrelated");

    expect(await listRefs(client(), BUCKET, CONTENT_ID, true)).toEqual(["a/legacy.mp4"]);
    expect(objects.has(refsKey(CONTENT_ID))).toBe(true);

    // The backfilled index answers the next call without another scan.
    const before = gets.filter((k) => k === mappingKey("a/other.mp4")).length;
    await listRefs(client(), BUCKET, CONTENT_ID, true);
    expect(gets.filter((k) => k === mappingKey("a/other.mp4")).length).toBe(before);
  });

  it("does not persist the backfill when asked not to", async () => {
    putMapping("a/legacy.mp4", CONTENT_ID);

    expect(await listRefs(client(), BUCKET, CONTENT_ID, false)).toEqual(["a/legacy.mp4"]);
    expect(objects.has(refsKey(CONTENT_ID))).toBe(false);
  });

  it("prefers the stored index over a scan", async () => {
    const refs: ContentRefs = {
      version: 1,
      contentId: CONTENT_ID,
      sourceKeys: ["a/from-index.mp4"],
      updatedAt: "2026-01-01T00:00:00Z",
    };
    objects.set(refsKey(CONTENT_ID), JSON.stringify(refs));
    putMapping("a/not-in-index.mp4", CONTENT_ID);

    expect(await listRefs(client(), BUCKET, CONTENT_ID, true)).toEqual(["a/from-index.mp4"]);
  });
});

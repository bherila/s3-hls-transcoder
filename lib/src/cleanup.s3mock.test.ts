import {
  DeleteObjectCommand,
  GetObjectCommand,
  ListObjectsV2Command,
  S3Client,
  type ListObjectsV2CommandInput,
} from "@aws-sdk/client-s3";
import { mockClient } from "aws-sdk-client-mock";
import { Readable } from "node:stream";
import { sdkStreamMixin } from "@smithy/util-stream";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { runCleanupPass } from "./cleanup.js";
import type { SourceMapping } from "./mapping.js";

const s3Mock = mockClient(S3Client);

const silentLogger = {
  debug: () => {},
  info: () => {},
  warn: () => {},
  error: () => {},
};

function bodyStream(payload: string) {
  return sdkStreamMixin(Readable.from([Buffer.from(payload)]));
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

beforeEach(() => s3Mock.reset());
afterEach(() => s3Mock.reset());

describe("runCleanupPass", () => {
  it("does not delete mappings owned by another source bucket in a shared destination", async () => {
    s3Mock.on(ListObjectsV2Command).callsFake((input: ListObjectsV2CommandInput) => {
      if (input.Bucket === "source-a") {
        return {
          Contents: [{ Key: "a/live.mp4", ETag: '"etag-a"', Size: 1, LastModified: new Date() }],
        };
      }
      return { Contents: [{ Key: "mappings/b/live-in-pair-B.mp4.json" }] };
    });
    s3Mock
      .on(GetObjectCommand, { Bucket: "dest", Key: "mappings/b/live-in-pair-B.mp4.json" })
      .resolves({ Body: bodyStream(JSON.stringify(mapping())) });

    const result = await runCleanupPass({
      sourceClient: new S3Client({}),
      destClient: new S3Client({}),
      sourceBucket: "source-a",
      sourceEndpoint: "https://source-a.example.com",
      destBucket: "dest",
      logger: silentLogger,
    });

    expect(result).toEqual({
      orphanMappingsFound: 0,
      orphanMappingsDeleted: 0,
      contentIdsGcd: 0,
      contentIdsRetained: 0,
      objectsDeleted: 0,
    });
    expect(s3Mock.commandCalls(DeleteObjectCommand)).toHaveLength(0);
  });

  it("deletes orphan mappings owned by the current source bucket", async () => {
    s3Mock.on(ListObjectsV2Command).callsFake((input: ListObjectsV2CommandInput) => {
      if (input.Bucket === "source-a") return { Contents: [] };
      if (input.Prefix === "mappings/") {
        return { Contents: [{ Key: "mappings/a/deleted.mp4.json" }] };
      }
      return { Contents: [] };
    });
    const ownedMappingBody = JSON.stringify(
      mapping({
        sourceKey: "a/deleted.mp4",
        sourceBucket: "source-a",
        sourceEndpoint: "https://source-a.example.com",
        contentId: "sha256:pair-a-content",
      }),
    );
    s3Mock
      .on(GetObjectCommand, { Bucket: "dest", Key: "mappings/a/deleted.mp4.json" })
      .resolvesOnce({ Body: bodyStream(ownedMappingBody) })
      .resolvesOnce({ Body: bodyStream(ownedMappingBody) });
    s3Mock.on(GetObjectCommand, { Bucket: "dest", Key: "fingerprints/index.json" }).resolves({
      Body: bodyStream(JSON.stringify({ version: 1, entries: [] })),
    });
    s3Mock.on(DeleteObjectCommand).resolves({});

    const result = await runCleanupPass({
      sourceClient: new S3Client({}),
      destClient: new S3Client({}),
      sourceBucket: "source-a",
      sourceEndpoint: "https://source-a.example.com",
      destBucket: "dest",
      logger: silentLogger,
    });

    expect(result.orphanMappingsFound).toBe(1);
    expect(result.orphanMappingsDeleted).toBe(1);
    expect(s3Mock.commandCalls(DeleteObjectCommand)).toContainEqual(
      expect.objectContaining({
        args: [
          expect.objectContaining({
            input: expect.objectContaining({ Key: "mappings/a/deleted.mp4.json" }),
          }),
        ],
      }),
    );
  });
});

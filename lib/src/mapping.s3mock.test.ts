import {
  GetObjectCommand,
  ListObjectsV2Command,
  PutObjectCommand,
  S3Client,
  S3ServiceException,
} from "@aws-sdk/client-s3";
import { mockClient } from "aws-sdk-client-mock";
import { Readable } from "node:stream";
import { sdkStreamMixin } from "@smithy/util-stream";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import {
  findMappingsForContentId,
  mappingKey,
  readMapping,
  writeMapping,
  type SourceMapping,
} from "./mapping.js";

const s3Mock = mockClient(S3Client);

function bodyStream(payload: string) {
  return sdkStreamMixin(Readable.from([Buffer.from(payload)]));
}

function notFound(): S3ServiceException {
  return new S3ServiceException({
    name: "NoSuchKey",
    $fault: "client",
    $metadata: { httpStatusCode: 404 },
    message: "Not Found",
  });
}

function makeMapping(sourceKey: string, contentId: string): SourceMapping {
  return {
    sourceKey,
    sourceBucket: "source",
    sourceEndpoint: "https://source.example.com",
    sourceEtag: "etag-" + sourceKey,
    sourceSize: 1024,
    sourceLastModified: "2026-01-01T00:00:00Z",
    contentId,
    hlsRoot: `by-id/${contentId}/`,
    encodedAt: "2026-01-01T00:00:00Z",
    encoderVersion: "0.1.0",
  };
}

beforeEach(() => s3Mock.reset());
afterEach(() => s3Mock.reset());

describe("readMapping / writeMapping", () => {
  it("returns null when the mapping object does not exist", async () => {
    s3Mock.on(GetObjectCommand).rejects(notFound());

    const result = await readMapping(new S3Client({}), "dest", "videos/a.mp4");
    expect(result).toBeNull();
  });

  it("parses a stored mapping body", async () => {
    const mapping = makeMapping("videos/a.mp4", "sha256:abc");
    s3Mock.on(GetObjectCommand).resolves({ Body: bodyStream(JSON.stringify(mapping)) });

    const result = await readMapping(new S3Client({}), "dest", "videos/a.mp4");
    expect(result).toEqual(mapping);

    const calls = s3Mock.commandCalls(GetObjectCommand);
    expect(calls[0]!.args[0].input.Key).toBe(mappingKey("videos/a.mp4"));
  });

  it("writes mapping JSON under the mappings/ prefix", async () => {
    s3Mock.on(PutObjectCommand).resolves({});

    const mapping = makeMapping("videos/a.mp4", "sha256:abc");
    await writeMapping(new S3Client({}), "dest", mapping);

    const calls = s3Mock.commandCalls(PutObjectCommand);
    expect(calls).toHaveLength(1);
    expect(calls[0]!.args[0].input.Key).toBe("mappings/videos/a.mp4.json");
    expect(calls[0]!.args[0].input.ContentType).toBe("application/json");
    const body = calls[0]!.args[0].input.Body as string;
    expect(JSON.parse(body)).toEqual(mapping);
  });

  it("propagates non-404 errors", async () => {
    const err = new S3ServiceException({
      name: "InternalError",
      $fault: "server",
      $metadata: { httpStatusCode: 500 },
      message: "boom",
    });
    s3Mock.on(GetObjectCommand).rejects(err);

    await expect(readMapping(new S3Client({}), "dest", "videos/a.mp4")).rejects.toThrow();
  });
});

describe("findMappingsForContentId", () => {
  it("returns only source keys whose mapping points at the content id", async () => {
    const target = "sha256:target";
    const other = "sha256:other";

    s3Mock.on(ListObjectsV2Command).resolves({
      Contents: [
        { Key: "mappings/a.mp4.json" },
        { Key: "mappings/b.mp4.json" },
        { Key: "mappings/c.mp4.json" },
        { Key: "mappings/skip.txt" },
      ],
    });

    s3Mock
      .on(GetObjectCommand, { Key: "mappings/a.mp4.json" })
      .resolves({ Body: bodyStream(JSON.stringify(makeMapping("a.mp4", target))) });
    s3Mock
      .on(GetObjectCommand, { Key: "mappings/b.mp4.json" })
      .resolves({ Body: bodyStream(JSON.stringify(makeMapping("b.mp4", other))) });
    s3Mock
      .on(GetObjectCommand, { Key: "mappings/c.mp4.json" })
      .resolves({ Body: bodyStream(JSON.stringify(makeMapping("c.mp4", target))) });

    const matches = await findMappingsForContentId(new S3Client({}), "dest", target);
    expect(matches.sort()).toEqual(["a.mp4", "c.mp4"]);
  });

  it("walks pagination via NextContinuationToken", async () => {
    s3Mock
      .on(ListObjectsV2Command)
      .resolvesOnce({
        Contents: [{ Key: "mappings/p1.mp4.json" }],
        NextContinuationToken: "tok",
      })
      .resolvesOnce({
        Contents: [{ Key: "mappings/p2.mp4.json" }],
      });

    s3Mock
      .on(GetObjectCommand, { Key: "mappings/p1.mp4.json" })
      .resolves({ Body: bodyStream(JSON.stringify(makeMapping("p1.mp4", "sha256:x"))) });
    s3Mock
      .on(GetObjectCommand, { Key: "mappings/p2.mp4.json" })
      .resolves({ Body: bodyStream(JSON.stringify(makeMapping("p2.mp4", "sha256:x"))) });

    const matches = await findMappingsForContentId(new S3Client({}), "dest", "sha256:x");
    expect(matches.sort()).toEqual(["p1.mp4", "p2.mp4"]);
    expect(s3Mock.commandCalls(ListObjectsV2Command)).toHaveLength(2);
  });

  it("returns empty when no objects exist", async () => {
    s3Mock.on(ListObjectsV2Command).resolves({});

    const matches = await findMappingsForContentId(new S3Client({}), "dest", "sha256:x");
    expect(matches).toEqual([]);
  });
});

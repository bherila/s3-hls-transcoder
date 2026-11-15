import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from "node:fs";
import { createRequire } from "node:module";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { URL } from "node:url";

function consumer(base, chain) {
  let require = createRequire(new URL(base, import.meta.url));
  for (const name of chain) require = createRequire(require.resolve(`${name}/package.json`));
  return require;
}

const containerRequire = consumer("../../integration/package.json", ["testcontainers"]);
const archiveRequire = createRequire(containerRequire.resolve("archiver/package.json"));
const archiveUtils = archiveRequire("archiver-utils");
const dockerRequire = createRequire(containerRequire.resolve("dockerode/package.json"));
const parserRequire = consumer("../../package.json", [
  "typescript-eslint",
  "@typescript-eslint/parser",
]);
const estreeRequire = createRequire(
  parserRequire.resolve("@typescript-eslint/typescript-estree/package.json"),
);

test("the container archive consumer preserves include, exclusion and file mapping semantics", () => {
  const directory = mkdtempSync(join(tmpdir(), "transcoder-dependency-"));
  try {
    mkdirSync(join(directory, "nested"));
    for (const file of ["first.txt", "ignored.txt", "nested/second.txt", "other.json"])
      writeFileSync(join(directory, file), "synthetic");
    assert.deepEqual(
      archiveUtils.file.expand({ cwd: directory }, ["**/*.txt", "!ignored.txt"]).sort(),
      ["first.txt", "nested/second.txt"],
    );
    const mapped = archiveUtils.file.expandMapping(["**/*.txt", "!ignored.txt"], "archive", {
      cwd: directory,
    });
    assert.deepEqual(mapped.map(({ dest }) => dest).sort(), [
      "archive/first.txt",
      "archive/nested/second.txt",
    ]);
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});

test("the real Dockerode UUID dependency preserves v4 and validates caller-provided buffers", () => {
  const uuid = dockerRequire("uuid");
  assert.equal(uuid.validate(uuid.v4()), true);
  assert.equal(uuid.version(uuid.v4()), 4);
  assert.throws(() => uuid.v5("synthetic", uuid.v5.DNS, new Uint8Array(1)), RangeError);
  assert.equal(uuid.v5("synthetic", uuid.v5.DNS), uuid.v5("synthetic", uuid.v5.DNS));
});

test("the actual TypeScript parser matcher bounds expansion while preserving file globs", () => {
  const { Minimatch, minimatch } = estreeRequire("minimatch");
  assert.equal(minimatch("lib/src/config.ts", "lib/**/*.ts"), true);
  assert.equal(minimatch("lib/src/config.js", "lib/**/*.ts"), false);
  assert.ok(new Minimatch("{a,b}".repeat(7), { braceExpandMax: 8 }).set.length <= 8);
  const parser = parserRequire("@typescript-eslint/parser");
  assert.equal(
    parser.parse('const value: string = "synthetic";', { sourceType: "module" }).body[0].type,
    "VariableDeclaration",
  );
});

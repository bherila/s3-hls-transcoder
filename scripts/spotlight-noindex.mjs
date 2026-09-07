import { closeSync, mkdirSync, openSync } from "node:fs";
import { resolve } from "node:path";
import process from "node:process";

if (process.platform === "darwin") {
  try {
    const nodeModulesPath = resolve("node_modules");

    mkdirSync(nodeModulesPath, { recursive: true });
    closeSync(openSync(resolve(nodeModulesPath, ".metadata_never_index"), "w"));
  } catch (error) {
    process.emitWarning(`Unable to create node_modules/.metadata_never_index: ${error.message}`);
  }
}

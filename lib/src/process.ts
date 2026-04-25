import { spawn, type SpawnOptions } from "node:child_process";

export interface ProcessResult {
  stdout: string;
  stderr: string;
}

/**
 * Runs a process to completion. Resolves on exit code 0; rejects with the
 * tail of stderr on any other exit. stdin is closed.
 */
export async function runProcess(
  cmd: string,
  args: string[],
  options: SpawnOptions = {},
): Promise<ProcessResult> {
  return new Promise((resolve, reject) => {
    const proc = spawn(cmd, args, { stdio: ["ignore", "pipe", "pipe"], ...options });
    let stdout = "";
    let stderr = "";
    proc.stdout?.on("data", (d) => {
      stdout += d.toString();
    });
    proc.stderr?.on("data", (d) => {
      stderr += d.toString();
    });
    proc.on("error", reject);
    proc.on("close", (code) => {
      if (code === 0) {
        resolve({ stdout, stderr });
      } else {
        const tail = stderr.length > 2000 ? `…${stderr.slice(-2000)}` : stderr;
        reject(new Error(`${cmd} exited ${code}\nstderr: ${tail}`));
      }
    });
  });
}

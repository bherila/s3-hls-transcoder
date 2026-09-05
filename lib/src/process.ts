import { spawn, type SpawnOptions } from "node:child_process";

export interface ProcessResult {
  stdout: string;
  stderr: string;
}

export interface RunProcessOptions extends SpawnOptions {
  timeoutMs?: number;
}

/**
 * Runs a process to completion. Resolves on exit code 0; rejects with the
 * tail of stderr on any other exit. stdin is closed.
 */
export async function runProcess(
  cmd: string,
  args: string[],
  options: RunProcessOptions = {},
): Promise<ProcessResult> {
  return new Promise((resolve, reject) => {
    const { timeoutMs, ...spawnOptions } = options;
    const proc = spawn(cmd, args, { stdio: ["ignore", "pipe", "pipe"], ...spawnOptions });
    let stdout = "";
    let stderr = "";
    let timedOut = false;
    const timer = timeoutMs
      ? setTimeout(() => {
          timedOut = true;
          proc.kill("SIGKILL");
        }, timeoutMs)
      : undefined;
    proc.stdout?.on("data", (d) => {
      stdout += d.toString();
    });
    proc.stderr?.on("data", (d) => {
      stderr += d.toString();
    });
    proc.on("error", reject);
    proc.on("close", (code) => {
      if (timer) clearTimeout(timer);
      if (timedOut) {
        reject(new Error(`${cmd} timed out after ${timeoutMs}ms`));
      } else if (code === 0) {
        resolve({ stdout, stderr });
      } else {
        const tail = stderr.length > 2000 ? `…${stderr.slice(-2000)}` : stderr;
        reject(new Error(`${cmd} exited ${code}\nstderr: ${tail}`));
      }
    });
  });
}

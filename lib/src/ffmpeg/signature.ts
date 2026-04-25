import { runProcess } from "../process.js";
import { findFfmpeg } from "./binary.js";

/**
 * Extracts an MPEG-7 video signature for perceptual matching. Writes a
 * binary signature blob to `outFile`. ffmpeg discards the encoded output
 * (`-f null -`); we only want the side-effect of the signature filter.
 *
 * The signature is robust to scaling and re-encoding, which is the property
 * we need for "is this a higher-resolution copy of an existing video?".
 */
export async function extractSignature(input: string, outFile: string): Promise<void> {
  await runProcess(findFfmpeg(), [
    "-y",
    "-i", input,
    "-vf", `signature=format=binary:filename=${outFile}`,
    "-f", "null",
    "-",
  ]);
}

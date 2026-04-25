export type ContentIdScheme = "sha256" | "psig";

const KNOWN_SCHEMES: ReadonlySet<ContentIdScheme> = new Set(["sha256", "psig"]);

export function formatContentId(scheme: ContentIdScheme, id: string): string {
  return `${scheme}:${id}`;
}

export function parseContentId(contentId: string): { scheme: ContentIdScheme; id: string } {
  const colon = contentId.indexOf(":");
  if (colon === -1) {
    throw new Error(`Content ID missing scheme prefix: ${contentId}`);
  }
  const scheme = contentId.slice(0, colon);
  const id = contentId.slice(colon + 1);
  if (!KNOWN_SCHEMES.has(scheme as ContentIdScheme)) {
    throw new Error(`Unknown content ID scheme: ${scheme}`);
  }
  return { scheme: scheme as ContentIdScheme, id };
}

export function byIdPrefix(contentId: string): string {
  return `by-id/${contentId}/`;
}

export function masterPlaylistKey(contentId: string): string {
  return `${byIdPrefix(contentId)}master.m3u8`;
}

export function metadataKey(contentId: string): string {
  return `${byIdPrefix(contentId)}metadata.json`;
}

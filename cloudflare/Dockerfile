# syntax=docker/dockerfile:1
#
# Cloudflare Containers image for the @s3-hls-transcoder/cloudflare entrypoint.
#
# Build from the repo root:
#   docker build -f cloudflare/Dockerfile -t s3-hls-transcoder-cf .
#
# Multi-arch:
#   docker buildx build -f cloudflare/Dockerfile --platform linux/amd64,linux/arm64 \
#       -t <registry>/s3-hls-transcoder-cf:latest --push .

ARG NODE_VERSION=22

# ----- Build stage --------------------------------------------------------
FROM node:${NODE_VERSION}-alpine AS builder

RUN apk add --no-cache build-base python3 git \
 && npm install -g pnpm@12.11.2

WORKDIR /workspace
COPY pnpm-workspace.yaml pnpm-lock.yaml package.json tsconfig.base.json ./
COPY lib ./lib
COPY cloudflare ./cloudflare

RUN pnpm install --frozen-lockfile
RUN pnpm --filter @s3-hls-transcoder/lib --filter @s3-hls-transcoder/cloudflare -r build
RUN pnpm --filter @s3-hls-transcoder/cloudflare deploy --prod /deploy

# ----- Runtime stage ------------------------------------------------------
FROM node:${NODE_VERSION}-bookworm-slim

# ffmpeg from Debian apt — fast, mainstream, multi-arch.
RUN set -eux \
 && apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=builder /deploy/dist ./dist
COPY --from=builder /deploy/node_modules ./node_modules
COPY --from=builder /deploy/package.json ./

ENTRYPOINT ["node", "dist/index.js"]

FROM golang:1.25.4-alpine AS build

WORKDIR /app/server
RUN apk add --no-cache build-base

# Copy dependency files first (changes less frequently)
COPY go.mod go.sum ./
RUN go mod download

# Copy only necessary files for build
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY api/ ./api/

ENV CGO_ENABLED=1 GOOS=linux
# Use build cache and mod cache mounts
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go build -o bot ./cmd/bot/main.go

FROM alpine:latest AS server

RUN apk update && apk add --no-cache ffmpeg yt-dlp
WORKDIR /app/server
COPY --from=build /app/server/bot ./
RUN chmod +x ./bot

CMD ["./bot"]
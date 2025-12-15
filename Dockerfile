FROM golang:1.25.4-alpine AS build

WORKDIR /app/server
# gopus (cgo) needs a C toolchain; install build-base (gcc, musl-dev)
RUN apk add --no-cache build-base
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Enable CGO so layeh.com/gopus can compile its C sources
ENV CGO_ENABLED=1 GOOS=linux
RUN go build -o bot ./cmd/bot/main.go

FROM alpine:latest AS server

RUN apk update && apk add --no-cache ffmpeg
WORKDIR /app/server
COPY --from=build /app/server/bot ./
RUN chmod +x ./bot

CMD ["./bot"]
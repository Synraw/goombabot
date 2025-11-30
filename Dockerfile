FROM golang:1.25.4-alpine AS build

WORKDIR /app/server
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o bot ./cmd/bot/main.go

FROM alpine:latest AS server

RUN apk update && apk add --no-cache ffmpeg
WORKDIR /app/server
COPY --from=build /app/server/bot ./
RUN chmod +x ./bot

CMD ["./bot"]
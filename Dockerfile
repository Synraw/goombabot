FROM golang:latest AS build

ARG METRICS_PORT="8080"

WORKDIR /app/server
COPY . .
RUN go mod download
RUN go build -o ./bot ./cmd/bot/main.go

FROM public.ecr.aws/amazonlinux/amazonlinux:latest AS server
WORKDIR /app/server
COPY --from=build /app/server/bot ./
RUN chmod +x ./bot

EXPOSE $METRICS_PORT

CMD [ "./bot" ]
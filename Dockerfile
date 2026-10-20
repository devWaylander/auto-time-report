# syntax=docker/dockerfile:1

FROM golang:1.27.3-alpine AS build
WORKDIR /src

# Зависимости отдельным слоем, чтобы не качать их на каждое изменение кода
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Драйвер SQLite на чистом Go, cgo не нужен
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bot ./cmd/bot

FROM alpine:3.22
# tzdata для TIMEZONE, ca-certificates для HTTPS к Telegram и Jira
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/bot /usr/local/bin/bot
ENTRYPOINT ["bot"]

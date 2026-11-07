# Stage 1: Build Go binary
FROM golang:alpine AS builder
ENV GOTOOLCHAIN=auto
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o sonar-bot ./cmd/bot/main.go

# Stage 2: Lightweight production runtime for Render Free Tier (512MB RAM)
FROM alpine:3.21
RUN apk add --no-cache ca-certificates ffmpeg nodejs python3 py3-pip curl && \
    pip3 install --no-cache-dir --break-system-packages -U "yt-dlp[default]"

WORKDIR /app
COPY --from=builder /app/sonar-bot /app/sonar-bot

EXPOSE 8080
CMD ["/app/sonar-bot"]

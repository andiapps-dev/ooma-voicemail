# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ooma-voicemail .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && \
    adduser -D -u 1000 ooma
USER ooma

COPY --from=builder /out/ooma-voicemail /usr/local/bin/ooma-voicemail

WORKDIR /data
VOLUME /data

ENTRYPOINT ["ooma-voicemail", "--state-file", "/data/voicemails.json", "--mp3-dir", "/data/voicemails"]

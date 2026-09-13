# frp-more: web UI manager running multiple embedded frpc instances in one container

FROM golang:1.25-alpine AS builder
WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o frp-more .

FROM alpine:3
RUN apk add --no-cache tzdata ca-certificates

COPY --from=builder /build/frp-more /usr/bin/frp-more

# Instance configs live here; mount a volume on it.
VOLUME ["/data"]
EXPOSE 1332

ENTRYPOINT ["/usr/bin/frp-more"]
CMD ["-data", "/data", "-addr", ":1332"]

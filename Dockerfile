FROM --platform=$BUILDPLATFORM golang:1.25-alpine3.21 AS builder
RUN apk add --no-cache ca-certificates
ARG TARGETOS
ARG TARGETARCH
WORKDIR /app
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /app/fsb -ldflags="-w -s" ./cmd/fsb

FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /app/fsb /app/fsb
EXPOSE ${PORT}
ENTRYPOINT ["/app/fsb", "run"]

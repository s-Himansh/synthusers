# syntax=docker/dockerfile:1
# Build stage: compile the daemon statically.
FROM golang:1.27-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o /out/synthusersd ./cmd/synthusersd

# Run stage: distroless static image with CA certificates for TLS probing.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/synthusersd /synthusersd
# Config is expected at /etc/synthusers/config.yaml (mount it).
EXPOSE 8080
ENTRYPOINT ["/synthusersd"]
CMD ["-config", "/etc/synthusers/config.yaml"]

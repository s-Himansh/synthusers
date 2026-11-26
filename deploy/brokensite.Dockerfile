# brokensite image: the same demo binary, built from the same source tree.
FROM golang:1.27-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o /out/brokensite ./cmd/brokensite

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/brokensite /brokensite
EXPOSE 6060 6161
ENTRYPOINT ["/brokensite"]

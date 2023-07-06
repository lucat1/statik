FROM golang:alpine AS go-builder
COPY . /build
WORKDIR /build
RUN go build -ldflags "-s -w" -o /build/statik

FROM scratch
COPY --from=go-builder /build/statik /usr/bin/statik

ENTRYPOINT ["/usr/bin/statik"]

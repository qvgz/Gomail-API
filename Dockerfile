FROM --platform=$BUILDPLATFORM golang:1.27 AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
  go mod download
COPY *.go ./
RUN --mount=type=cache,target=/go/pkg/mod \
  --mount=type=cache,target=/root/.cache/go-build \
  CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
  go build -trimpath -buildvcs=false \
  -ldflags="-s -w -buildid= -X main.Version=${VERSION}" \
  -o /out/gomail-api .

FROM gcr.io/distroless/static:nonroot
WORKDIR /app
COPY --from=builder /out/gomail-api /app/gomail-api
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/app/gomail-api"]

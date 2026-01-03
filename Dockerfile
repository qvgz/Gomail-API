
FROM --platform=$BUILDPLATFORM golang:latest AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /app
COPY go.mod ./
RUN go mod download
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
  go build -trimpath -buildvcs=false \
  -ldflags="-s -w -buildid= -X main.Version=${VERSION}" \
  -o gomail-api ./main.go

FROM gcr.io/distroless/static-debian13:nonroot
WORKDIR /app
COPY --from=builder /app/gomail-api /app/gomail-api
EXPOSE 8080
ENTRYPOINT ["/app/gomail-api"]
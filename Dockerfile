# Image for in-cluster mode (kp silent serve). Multi-arch via buildx.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X github.com/lyelgate/kubepilot/internal/app.Version=${VERSION}" -o /out/kp ./cmd/kp

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/kp /kp
USER nonroot:nonroot
ENTRYPOINT ["/kp"]
CMD ["silent", "serve"]

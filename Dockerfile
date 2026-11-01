# Image for in-cluster mode (kdeck silent serve). Multi-arch via buildx.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X github.com/lyelgate/kdeck/internal/app.Version=${VERSION}" -o /out/kdeck ./cmd/kdeck

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/kdeck /kdeck
USER nonroot:nonroot
ENTRYPOINT ["/kdeck"]
CMD ["silent", "serve"]

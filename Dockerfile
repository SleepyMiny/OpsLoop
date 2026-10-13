# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27.2 AS build
ARG TARGETOS TARGETARCH
ARG APP=apps/demo-api
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/app ./$APP
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
USER 65532:65532
ENTRYPOINT ["/app"]

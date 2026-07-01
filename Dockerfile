# Multi-stage build -> small static single binary (distribution: single binary +
# image, per the project plan). Base layers are ordered so dependency download
# caches independently of source changes (article lever #4).
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/heave ./cmd/heave

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/heave /heave
COPY config.example.yaml /config.yaml
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/heave", "-config", "/config.yaml"]

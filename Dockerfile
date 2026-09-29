# syntax=docker/dockerfile:1
FROM golang:1.27.1-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
COPY internal ./internal
# Embed timezone data for the scratch runtime; no shell or compiler is shipped.
RUN CGO_ENABLED=0 go build -tags timetzdata -trimpath -ldflags='-s -w' -o /out/canvaslink .

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/canvaslink /canvaslink
USER 65532:65532
ENV CANVASLINK_OAUTH_LISTEN_ADDR=:9090
EXPOSE 9090
ENTRYPOINT ["/canvaslink"]

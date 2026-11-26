FROM golang:1.27.4-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -p=2 -trimpath -ldflags="-s -w" -o /out/cloudreve-s3 ./cmd/cloudreve-s3
RUN install -d -m 0700 /out/state

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/cloudreve-s3 /app/cloudreve-s3
COPY third-party /app/third-party
COPY --from=build --chown=65532:65532 /out/state /state
EXPOSE 7070
USER 65532:65532
ENTRYPOINT ["/app/cloudreve-s3"]
CMD ["serve", "-config", "/config/gateway.json"]

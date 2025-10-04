# Dockerfile
FROM golang:1.25 as build
WORKDIR /src
COPY . .
RUN go mod init rssgotransmission || true \
 && go get github.com/mmcdole/gofeed \
 && go build -o /out/rssgotransmission .

FROM gcr.io/distroless/base-debian12
WORKDIR /app
COPY --from=build /out/rssgotransmission /app/
VOLUME ["/data"]
ENV INTERVAL=15m STATE_PATH=/data/state.json
ENTRYPOINT ["/app/rssgotransmission"]

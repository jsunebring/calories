# ── build ──────────────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /calorie-log .

# ── runtime ────────────────────────────────────────────────────────────────
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && mkdir -p /data
COPY --from=build /calorie-log /usr/local/bin/calorie-log
ENV DATA_DIR=/data PORT=8080
EXPOSE 8080
VOLUME ["/data"]
ARG GIT_COMMIT=unknown
LABEL org.opencontainers.image.revision="$GIT_COMMIT"
ENTRYPOINT ["/usr/local/bin/calorie-log"]

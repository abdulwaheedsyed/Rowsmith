# syntax=docker/dockerfile:1.7
# Rowsmith: one static binary with the web UI embedded, on a distroless base.

FROM node:26-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM golang:1.27-bookworm AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-buildvcs=false
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY --from=web /web/dist ./internal/web/dist
ARG VERSION=dev
RUN go build -trimpath -ldflags "-s -w" -o /out/rowsmith ./cmd/rowsmith \
 && mkdir -p /out/data /out/tmp

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/rowsmith /rowsmith
COPY LICENSE NOTICE THIRD_PARTY_NOTICES.txt /licenses/
COPY --from=build --chown=65532:65532 /out/data /data
COPY --from=build --chown=65532:65532 /out/tmp /tmp
ENV ROWSMITH_DATA_DIR=/data ROWSMITH_ADDR=:8080
USER 65532:65532
EXPOSE 8080
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/rowsmith", "healthcheck"]
ENTRYPOINT ["/rowsmith"]
CMD ["serve"]

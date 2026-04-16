# -------------------------------
FROM golang:1.24.1 AS dev

# GH Actions sets these variables
ARG GIT_TAG=unknown

RUN go install github.com/swaggo/swag/cmd/swag@v1.16.6

# Generate schema docs at build time using json-schema-for-humans
RUN apt-get update && apt-get install -y python3 python3-venv python3-pip && rm -rf /var/lib/apt/lists/*

# Hot-Reloader
RUN go install github.com/githubnemo/CompileDaemon@v1.4.0

COPY ./ /app
WORKDIR /app

RUN chmod +x ./scripts/generate-schema-docs.sh && \
    ./scripts/generate-schema-docs.sh && \
    swag init && \
    go build -ldflags="-X main.GitTag=${GIT_TAG}" -o main main.go


ENTRYPOINT ["CompileDaemon", "--build=go build main.go", "--command=./main"]
# -------------------------------

# -------------------------------
FROM debian:12.8-slim AS prod

# Install CA certificates and other runtime dependencies
RUN apt-get update && apt-get install -y \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=dev /app/main /app/main
COPY --from=dev /app/public /app/public
COPY --from=dev /app/views /app/views
COPY --from=dev /app/docs /app/docs

ENTRYPOINT ["/app/main"]
# -------------------------------

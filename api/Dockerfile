# -------------------------------
FROM golang:1.23.2 AS dev

# RUN go install github.com/swaggo/swag/cmd/swag@v1.8.12

# Hot-Reloader
RUN go install github.com/githubnemo/CompileDaemon@v1.4.0

COPY ./ /app
WORKDIR /app

# RUN swag init
RUN go build main.go

ENTRYPOINT ["CompileDaemon", "--build=go build main.go", "--command=./main"]
# -------------------------------

# -------------------------------
FROM debian:12.6-slim as prod

# Copy the CA certificates from the dev stage
COPY --from=dev /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

WORKDIR /app

COPY --from=dev /app/main /app/main
COPY --from=dev /app/public /app/public
COPY --from=dev /app/views /app/views
COPY --from=dev /app/docs /app/docs

ENTRYPOINT ["/app/main"]
# -------------------------------

# Tiny Go reverse proxy that fronts hermes' dashboard with
# auth.romaine.life cookie delegation. See auth-proxy/README.md.

FROM golang:1.23-alpine AS build
WORKDIR /src

# Cache deps separately when there are some.
COPY auth-proxy/go.mod auth-proxy/go.sum* ./
RUN if [ -f go.sum ]; then go mod download; fi

COPY auth-proxy/. .

# CGO off so the binary is fully static for the distroless runtime.
ENV CGO_ENABLED=0
RUN go test ./... && \
    go build -trimpath -ldflags="-s -w" -o /out/auth-proxy .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/auth-proxy /usr/local/bin/auth-proxy
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/auth-proxy"]

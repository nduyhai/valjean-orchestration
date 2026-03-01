# ── Build stage ───────────────────────────────────────────────────────────────
FROM golang:1.26-alpine AS build
WORKDIR /src

# Cache deps separately from source
COPY go.mod go.sum ./
RUN go mod download

# Copy all source (main.go is at repo root)
COPY . .

# Static binary — strip debug symbols
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o /out/valjean .

# ── Runtime stage ─────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=build /out/valjean /valjean
ENV ADDR=:8080
EXPOSE 8080
ENTRYPOINT ["/valjean"]

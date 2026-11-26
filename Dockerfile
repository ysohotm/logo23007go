# Build
FROM golang:1.24-bookworm AS build
ENV GOTOOLCHAIN=auto
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /out/logo23007go .

# Runtime
FROM debian:bookworm-slim AS final
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /out/logo23007go /app/logo23007go
COPY app_data /app/app_data
EXPOSE 23107
ENV PORT=23107 TZ=Asia/Shanghai
ENTRYPOINT ["/app/logo23007go"]

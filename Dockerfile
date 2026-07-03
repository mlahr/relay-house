FROM golang:1.24-bookworm AS build

WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/relay-house ./cmd/server

FROM debian:bookworm-slim

RUN useradd --system --create-home --home-dir /app app
WORKDIR /app
COPY --from=build /out/relay-house /usr/local/bin/relay-house
USER app

EXPOSE 8080
CMD ["relay-house"]

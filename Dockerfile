FROM golang:1.24-bookworm AS build

WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/email-endpoint ./cmd/server

FROM debian:bookworm-slim

RUN useradd --system --create-home --home-dir /app app
WORKDIR /app
COPY --from=build /out/email-endpoint /usr/local/bin/email-endpoint
USER app

EXPOSE 8080
CMD ["email-endpoint"]

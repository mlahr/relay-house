APP_NAME := relay-house
PREFIX ?= /usr/local
BINDIR ?= $(PREFIX)/bin

.PHONY: all build clean fmt help install test tidy vet

all: fmt vet test build

help:
	@printf '%s\n' \
		'Default target: all' \
		'' \
		'Targets:' \
		'  all        Run fmt, vet, test, and build' \
		'  build      Build ./$(APP_NAME)' \
		'  clean      Remove build and coverage artifacts' \
		'  fmt        Format Go source files' \
		'  install    Install $(APP_NAME) to $(BINDIR)' \
		'  test       Run all Go tests' \
		'  tidy       Tidy Go module dependencies' \
		'  vet        Run go vet'

build:
	go build -o $(APP_NAME) ./cmd/server

clean:
	go clean
	rm -f coverage.out coverage.html $(APP_NAME)

fmt:
	go fmt ./...

install: build
	install -d $(BINDIR)
	install -m 0755 $(APP_NAME) $(BINDIR)/$(APP_NAME)

test:
	go test ./...

tidy:
	go mod tidy

vet:
	go vet ./...

# CLIENT_ID is the GitHub OAuth App's client id (see README). It is public by
# design: the device flow is used precisely because it needs no client secret.
CLIENT_ID ?=

BIN      := bin/herdr-huddle
LDFLAGS  := -s -w
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null)
ifneq ($(VERSION),)
LDFLAGS  += -X main.version=$(VERSION)
endif
ifneq ($(CLIENT_ID),)
LDFLAGS  += -X main.clientID=$(CLIENT_ID)
endif

.PHONY: build test check install clean

build:
	go build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/herdr-huddle

test:
	go test -count=1 ./...

# What CI would run, and what to run before committing.
check:
	test -z "$$(gofmt -l .)"
	go vet ./...
	go test -race -count=1 ./...

# Prints the link command rather than running it: registering a plugin is a
# change to your Herdr, and this repository does not make those for you.
install: build
	@echo "built $(BIN). Now run:"
	@echo "  herdr plugin link $(CURDIR)"

clean:
	rm -rf bin

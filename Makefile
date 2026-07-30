BINARIES := chmura chmura-server chmura-agent chmura-dev
VERSION  ?= 0.0.0-dev
LDFLAGS  := -X github.com/daropotter/chmura/internal/version.Version=$(VERSION)

.PHONY: test vet build clean $(BINARIES)

test:
	go test ./...

vet:
	go vet ./...

build: $(BINARIES)

$(BINARIES):
	go build -ldflags "$(LDFLAGS)" -o bin/$@ ./cmd/$@

clean:
	rm -rf bin/

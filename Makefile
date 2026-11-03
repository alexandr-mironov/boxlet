BIN := bin

.PHONY: build clean run

build:
	mkdir -p $(BIN)
	go build -o $(BIN)/boxlet ./cmd/boxlet
	go build -o $(BIN)/boxletd ./cmd/boxletd
	@echo "готово: $(BIN)/boxlet, $(BIN)/boxletd"

run: build
	$(BIN)/boxlet start

clean:
	rm -rf $(BIN)

vet:
	go vet ./...

tidy:
	go mod tidy

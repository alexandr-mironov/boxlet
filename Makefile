BIN := bin

.PHONY: build clean run vet tidy vm-helper

build:
	mkdir -p $(BIN)
	go build -o $(BIN)/boxlet ./cmd/boxlet
	go build -o $(BIN)/boxletd ./cmd/boxletd
ifeq ($(shell uname -s),Darwin)
	$(MAKE) vm-helper
	go build -o $(BIN)/boxlet-priv ./cmd/boxlet-priv
endif
	@echo "готово: $(BIN)/boxlet, $(BIN)/boxletd$(if $(filter Darwin,$(shell uname -s)), $(BIN)/boxlet-vm, $(BIN)/boxlet-priv,)"

# vm-helper собирает Swift-хелпер boxlet-vm (Virtualization.framework)
# и подписывает его ad-hoc с entitlement на виртуализацию.
vm-helper:
	mkdir -p $(BIN)
	swiftc -O -o $(BIN)/boxlet-vm vmhelper/boxlet-vm.swift -framework Virtualization
	codesign -s - --entitlements vmhelper/entitlements.plist $(BIN)/boxlet-vm

run: build
	$(BIN)/boxlet start

clean:
	rm -rf $(BIN)

vet:
	go vet ./...

tidy:
	go mod tidy

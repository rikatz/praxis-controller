.PHONY: build test vet generate

build:
	mkdir -p bin
	go build -trimpath -o bin/praxis-controller ./cmd/controller

test:
	go test ./...

vet:
	go vet ./...

generate:
	go tool controller-gen rbac:roleName=manager-role paths=./internal/controller output:rbac:dir=config/rbac

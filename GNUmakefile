default: fmt build test

build:
	go build ./...

install: build
	go install .

lint:
	go vet ./...
	staticcheck ./...

# docs/ from the schema and examples/; needs terraform on PATH.
generate:
	cd tools && go generate ./...

fmt:
	gofmt -s -w .
	terraform fmt -recursive examples/

test:
	go test -race ./...

# Acceptance tests: real terraform against an in-process fake of the API.
testacc:
	TF_ACC=1 CHECKPOINT_DISABLE=1 go test -race ./...

.PHONY: default build install lint generate fmt test testacc

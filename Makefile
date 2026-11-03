.PHONY: test image
test:
	go test ./...

# Cross-compiles on the host, then packages the static binary.
image:
	GOOS=linux GOARCH=$(shell podman info --format '{{.Host.Arch}}') CGO_ENABLED=0 go build -o build/orchivio-worker ./cmd/orchivio-worker
	podman build -t localhost/orchivio-worker:dev .

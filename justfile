set shell := ["bash", "-uc"]

default:
	@just --list

# Run the mock on :8080
run:
	go run ./cmd

# Build the image and run it on :8080 with a config file
docker config="config.example.yaml":
	docker build -t flowroute:dev .
	docker run --rm -d --name flowroute -p 8080:8080 -v "$(realpath {{config}})":/config/config.yaml:ro flowroute:dev

test:
	go test -race -count=1 ./...

ci:
	test -z "$(gofmt -l .)"
	go vet ./...
	go test -race -count=1 ./...
	golangci-lint run ./...

down:
	docker stop flowroute 2>/dev/null || true

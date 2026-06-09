.PHONY: build run dev clean tidy fireworks-service

build:
	go build -o bin/reg-server cmd/server/main.go

run: build
	./bin/reg-server --config configs/config.yaml

dev:
	go run cmd/server/main.go --config configs/config.yaml

clean:
	rm -rf bin/

# Fireworks Python service
fireworks-service:
	cd scripts && pip install -r requirements.txt && python fireworks_reg.py --port 5000

tidy:
	go mod tidy

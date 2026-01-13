run-local:
	@echo "Start local development"
	REDIS_TYPE=master \
	REDIS_ADDRESS=localhost \
	REDIS_PORT=6379
	go run main.go
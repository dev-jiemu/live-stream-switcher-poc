include .env
export

run-local:
	@echo "Starting local development :)"
	@exec go run main.go

run-local-a:
	@echo "Starting server-a :)"
	@exec go run main.go -id server-a -port 1935

run-local-b:
	@echo "Starting server-b :)"
	@exec go run main.go -id server-b -port 1936
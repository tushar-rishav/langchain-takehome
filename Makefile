.PHONY: db-up db-down db-migrate db-downgrade server format lint deps test test-setup db-migrate-test

# Configuration
DB_URL ?= postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable
DB_URL_TEST ?= postgres://postgres:postgres@localhost:5432/postgres_test?sslmode=disable
MIGRATIONS_DIR := migrations

# Install dependencies
deps:
	go mod tidy

# Start all database services defined in docker-compose-db.yaml
db-up:
	docker-compose -f docker-compose-db.yaml up -d

# Stop all database services defined in docker-compose-db.yaml
db-down:
	docker-compose -f docker-compose-db.yaml down

# Run database migrations (requires golang-migrate to be installed)
db-migrate:
	migrate -path $(MIGRATIONS_DIR) -database "$(DB_URL)" up

# Run migrations with test environment
db-migrate-test:
	migrate -path $(MIGRATIONS_DIR) -database "$(DB_URL_TEST)" up

# Revert the most recent migration
db-downgrade:
	migrate -path $(MIGRATIONS_DIR) -database "$(DB_URL)" down 1

# Run the Go server locally
server:
	go run ./cmd/server

# Format the codebase
fmt:
	@gofmt -s -w .

# Basic linting using go vet
lint:
	@go vet ./...

# Set up test environment (reset database and S3 bucket)
test-setup:
	@echo "Setting up test environment..."
	@echo "1. Dropping postgres_test database if it exists..."
	-docker exec -it ls-go-run-handler-db-postgres-15-1 psql -U postgres -c "DROP DATABASE IF EXISTS postgres_test;"
	@echo "2. Creating postgres_test database..."
	docker exec -it ls-go-run-handler-db-postgres-15-1 psql -U postgres -c "CREATE DATABASE postgres_test;"
	@echo "3. Configuring MinIO client..."
	docker exec -it ls-go-run-handler-minio-1 mc alias set local http://localhost:9000 minioadmin1 minioadmin1
	@echo "4. Clearing and recreating runs-test bucket..."
	-docker exec -it ls-go-run-handler-minio-1 mc rb --force local/runs-test
	docker exec -it ls-go-run-handler-minio-1 mc mb local/runs-test
	@echo "5. Running migrations on test database..."
	make db-migrate-test
	@echo "Test environment setup complete!"

# Run tests with test environment
test: test-setup
	RUN_HANDLER_ENV=test go test ./...
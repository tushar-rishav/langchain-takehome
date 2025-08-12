# ls-go-run-handler

A starter Go server with endpoints for ingesting and fetching runs.

## Features

- `POST /runs` endpoint to create new runs
- `GET /runs/{id}` endpoint to retrieve run information by UUID

## Quick Start

Ensure you have [Docker](https://docker.com) and  [Go](https://go.dev) installed.

```bash
# 1) Start local Postgres and MinIO services (uses docker-compose-db.yaml)
make db-up

# 2) Install golang-migrate CLI (macOS)
brew install golang-migrate
# alternative: using Go toolchain (requires CGO/postgres tags)
# go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest

# 3) Run DB migrations
make db-migrate

# 4) Install dependencies
make deps
# or
# go mod tidy

# 5) Run the server, creating the `runs` bucket if needed.
make server
# or
# go run ./cmd/server  # <-- this will FAIL on requests if the `runs` bucket doesn't exist
```

The API will be available at http://localhost:8000

### Example API Usage

#### Creating Runs

```bash
# Create a new run
curl -X POST http://localhost:8000/runs \
  -H "Content-Type: application/json" \
  -d '[
    {
      "trace_id": "944ce838-b5c5-4628-8f23-089fbda8b9e3",
      "name": "Weather Query",
      "inputs": {"query": "What is the weather in San Francisco?"},
      "outputs": {"response": "It is currently 65°F and sunny in San Francisco."},
      "metadata": {"model": "gpt-4", "temperature": 0.7, "tokens": 42}
    }
  ]'
```

Response:
```json
{
  "ids": ["<generated-uuid>"]
}
```

#### Retrieving a Run

```bash
# Get a run by ID (replace <run-id> with an actual UUID)
curl -X GET http://localhost:8000/runs/<run-id>
```

Response:
```json
{
  "id": "<run-id>",
  "trace_id": "944ce838-b5c5-4628-8f23-089fbda8b9e3",
  "name": "Weather Query",
  "inputs": {"query": "What is the weather in San Francisco?"},
  "outputs": {"response": "It is currently 65°F and sunny in San Francisco."},
  "metadata": {"model": "gpt-4", "temperature": 0.7, "tokens": 42}
}
```

## Setup Details

Requirements:
- Go 1.22+
- Docker (for Postgres/MinIO via docker-compose)
- golang-migrate (CLI) for database migrations

Install deps (optional; `go run` will fetch automatically):
```bash
go mod download
```

## Database Setup

This project uses PostgreSQL for data storage and MinIO for object storage. Docker Compose is used to manage these services.

```bash
# Start database services (PostgreSQL and MinIO)
make db-up

# Stop database services
make db-down

# Run database migrations (requires golang-migrate CLI)
make db-migrate

# Revert the most recent migration
make db-downgrade
```

## Running the Server

```bash
# Start the server
make server

# Or manually start the server
go run ./cmd/server

# Use a custom port (likely not needed)
PORT=8080 go run ./cmd/server
```

## Linting and Formatting

```bash
# Format code (gofmt)
make format

# Basic linting (go vet)
make lint
```

## Testing

The project uses Go's built-in testing package and includes a dedicated test environment configuration.

Run tests (this automatically sets up the test environment):

```bash
make test
```

The test command will:

- Set up a clean test environment (drop and recreate the test database and S3 bucket)
- Run migrations on the test database
- Execute all tests with the test environment settings

### Test Environment Setup

The test environment uses:

- A separate database: `postgres_test`
- A separate S3 bucket: `runs-test`
- Environment variables from `.env.test` (or sensible defaults if the file is missing)

You can manually set up the test environment without running tests:

```bash
make test-setup
```

### Environment Configuration

The application uses environment-specific configuration:

- Development: Uses the default `.env` file
- Testing: Uses the `.env.test` file when `RUN_HANDLER_ENV=test` is set

The Makefile targets for tests and benchmarks set `RUN_HANDLER_ENV=test` automatically so tests run with isolated resources without affecting your development environment.

## Benchmarking and Profiling

The project includes tools for performance benchmarking and memory allocation profiling to help identify bottlenecks and optimize resource usage.

### Performance Benchmarks

Performance benchmarks measure execution time of key operations using Go's `testing` benchmarks. The benchmarks are designed to isolate HTTP request handling from data preparation; large request bodies are generated once outside the timed loop.

Run performance benchmarks:

```bash
make bench
```

This will:

- Set up a clean test environment
- Run the benchmark tests with `-benchmem` to report memory allocations

Example benchmark scenarios (as currently configured):

- Processing 500 runs with 100KB of data per field (inputs/outputs/metadata)
- Processing 50 runs with 1000KB (1MB) of data per field

You can adjust scenarios by editing `cmd/server/bench_test.go`.

Tip: To run benchmarks directly without Make:

```bash
RUN_HANDLER_ENV=test go test -bench=. -benchmem -run='^$' ./...
```

# ls-go-run-handler

An interview starter Go server with endpoints for ingesting and fetching runs.

## Features

- `POST /runs` endpoint to create new runs
- `GET /runs/{id}` endpoint to retrieve run information by UUID

## Quick Start

```bash
# 1) Start local Postgres (and MinIO, though it's not used yet)
make db-up

# 2) Install golang-migrate CLI (macOS)
brew install golang-migrate
# alternative: using Go toolchain (requires CGO/postgres tags)
# go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest

# 3) Run DB migrations
make db-migrate

# 4) Run the server
make server
# or
# go run ./cmd/server
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

Configuration:
- Default DB URL: `postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable`
- Override via `DB_URL` variable, e.g.:
  ```bash
  make db-migrate DB_URL="postgres://user:pass@localhost:5432/mydb?sslmode=disable"
  ```

Alternative migrations via Docker (no local install):
```bash
docker run --rm \
  -v "$(pwd)/migrations:/migrations" \
  --network host \
  migrate/migrate \
  -path=/migrations -database "$DB_URL" up
```

## Running the Server

```bash
# Start the server
make server

# Or manually start the server
go run ./cmd/server

# Use a custom port
PORT=8080 go run ./cmd/server
```

## Linting and Formatting

```bash
# Format code (gofmt)
make format

# Basic linting (go vet)
make lint
```

## API Documentation

This starter does not include Swagger/ReDoc. Use the examples below to exercise endpoints.

## Testing

No tests are included yet. Add tests under `./...` and run with:
```bash
go test ./...
```

## Project Structure

```
.
├── cmd
│   └── server
│       └── main.go         # chi-based HTTP server with mock endpoints
├── migrations              # golang-migrate SQL migrations
│   ├── 0001_create_runs_table.up.sql
│   └── 0001_create_runs_table.down.sql
├── docker-compose-db.yaml  # Postgres + MinIO services
├── Makefile                # common dev tasks
└── go.mod                  # module definition
```

Notes:
- Endpoints implemented with mock responses for now:
  - POST /runs
  - GET /runs/{id}
- MinIO is included for parity with the Python version but unused by the Go code currently.
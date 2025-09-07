package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	appconfig "github.com/langchain-ai/ls-go-run-handler/internal/config"
)

// RunIn represents input payload for a run.
type RunIn struct {
	ID       *string        `json:"id,omitempty"`
	TraceID  string         `json:"trace_id"`
	Name     string         `json:"name"`
	Inputs   map[string]any `json:"inputs"`
	Outputs  map[string]any `json:"outputs"`
	Metadata map[string]any `json:"metadata"`
}

// runJSON is used to produce stable JSON for batch upload while tracking offsets.
type runJSON struct {
	ID       uuid.UUID       `json:"id"`
	TraceID  uuid.UUID       `json:"trace_id"`
	Name     string          `json:"name"`
	Inputs   json.RawMessage `json:"inputs"`
	Outputs  json.RawMessage `json:"outputs"`
	Metadata json.RawMessage `json:"metadata"`
}

type Server struct {
	cfg appconfig.Settings
	dsn string
	s3  *s3.Client
	db  *pgxpool.Pool
}

// bufferPool is used to reuse buffers for batch JSON construction
var bufferPool = sync.Pool{
	New: func() any {
		return new(bytes.Buffer)
	},
}

func main() {
	ctx := context.Background()

	// Load settings
	settings := appconfig.Load()

	// Build DSN for Postgres
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%s/%s", settings.DBUser, settings.DBPassword, settings.DBHost, settings.DBPort, settings.DBName)

	// Init S3 client (communicate to MinIO locally)
	awsCfg, err := awsconfig.LoadDefaultConfig(
		ctx,
		awsconfig.WithRegion(settings.S3Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(settings.S3AccessKey, settings.S3SecretKey, "")),
	)
	if err != nil {
		log.Fatalf("failed to load AWS config: %v", err)
	}
	s3Client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.UsePathStyle = true
		o.BaseEndpoint = aws.String(settings.S3Endpoint)
	})

	dbpool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("failed to create db pool: %v", err)
	}
	defer dbpool.Close()
	srv := &Server{cfg: settings, dsn: dsn, s3: s3Client, db: dbpool}

	r := chi.NewRouter()
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	r.Post("/runs", srv.createRunsHandler)
	r.Get("/runs/{id}", srv.getRunHandler)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8000"
	}
	addr := ":" + port
	log.Printf("Starting server on %s", addr)
	if err := http.ListenAndServe(addr, r); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

// createRunsHandler accepts a payload of runs, uploads a batch JSON to S3 for large fields, and stores S3 refs in Postgres.
func (s *Server) createRunsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	ctx := r.Context()

	// Parse runs. NOTE: feel free to change the format of the payload
	var runs []RunIn
	if err := json.NewDecoder(r.Body).Decode(&runs); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON body, expected an array of runs"})
		return
	}
	if len(runs) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "No runs provided"})
		return
	}

	batchID := uuid.New().String()
	objectKey := fmt.Sprintf("batches/%s.json", batchID)

	type runOffsets struct {
		id          uuid.UUID
		traceID     uuid.UUID
		name        string
		inputsRef   string
		outputsRef  string
		metadataRef string
	}

	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	buf.WriteByte('[')
	offs := make([]runOffsets, 0, len(runs))

	for i, in := range runs {
		// id
		var id uuid.UUID
		if in.ID != nil && *in.ID != "" {
			var err error
			id, err = uuid.Parse(*in.ID)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("invalid id at index %d", i)})
				return
			}
		} else {
			id = uuid.New()
		}
		// trace_id
		traceID, err := uuid.Parse(in.TraceID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("invalid trace_id at index %d", i)})
			return
		}

		// Marshal nested fields first to preserve bytes
		inputsBytes, _ := json.Marshal(in.Inputs)
		outputsBytes, _ := json.Marshal(in.Outputs)
		metadataBytes, _ := json.Marshal(in.Metadata)
		rj := runJSON{ID: id, TraceID: traceID, Name: in.Name, Inputs: inputsBytes, Outputs: outputsBytes, Metadata: metadataBytes}
		runBytes, err := json.Marshal(rj)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "failed to encode run"})
			return
		}
		if i > 0 {
			buf.WriteByte(',')
		}
		runStart := buf.Len()
		buf.Write(runBytes)

		mkRef := func(field string, fieldBytes []byte) string {
			idx := bytes.Index(runBytes, fieldBytes)
			if idx == -1 {
				return ""
			}
			start := runStart + idx
			end := start + len(fieldBytes)
			return fmt.Sprintf("s3://%s/%s#%d:%d/%s", s.cfg.S3BucketName, objectKey, start, end, field)
		}

		offs = append(offs, runOffsets{
			id:          id,
			traceID:     traceID,
			name:        in.Name,
			inputsRef:   mkRef("inputs", inputsBytes),
			outputsRef:  mkRef("outputs", outputsBytes),
			metadataRef: mkRef("metadata", metadataBytes),
		})
	}
	buf.WriteByte(']')
	// Return buffer to pool after use
	defer bufferPool.Put(buf)

	// Upload batch JSON
	_, err := s.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.cfg.S3BucketName),
		Key:         aws.String(objectKey),
		Body:        bytes.NewReader(buf.Bytes()),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		log.Printf("s3 PutObject error: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "failed to upload batch to object storage"})
		return
	}

	// Insert references into Postgres
	conn, err := s.db.Acquire(ctx)
	if err != nil {
		log.Printf("db acquire error: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "failed to acquire database connection"})
		return
	}
	defer conn.Release()

	runIDs := make([]string, 0, len(offs))
	tx, err := conn.Begin(ctx)
	if err != nil {
		log.Printf("db tx begin error: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "failed to begin transaction"})
		return
	}
	defer tx.Rollback(ctx)

	// Build multi-row INSERT
	valueStrings := make([]string, 0, len(offs))
	valueArgs := make([]any, 0, len(offs)*6)
	for i, ro := range offs {
		valueStrings = append(valueStrings, fmt.Sprintf("($%d,$%d,$%d,$%d,$%d,$%d)", i*6+1, i*6+2, i*6+3, i*6+4, i*6+5, i*6+6))
		valueArgs = append(valueArgs, ro.id, ro.traceID, ro.name, ro.inputsRef, ro.outputsRef, ro.metadataRef)
	}
	insertSQL := fmt.Sprintf(`INSERT INTO runs (id, trace_id, name, inputs, outputs, metadata) VALUES %s RETURNING id`, strings.Join(valueStrings, ","))
	rows, err := tx.Query(ctx, insertSQL, valueArgs...)
	if err != nil {
		log.Printf("db batch insert error: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "failed to batch insert runs"})
		return
	}
	defer rows.Close()
	for rows.Next() {
		var outID uuid.UUID
		if err := rows.Scan(&outID); err != nil {
			log.Printf("db scan error: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "failed to scan inserted run id"})
			return
		}
		runIDs = append(runIDs, outID.String())
	}
	if err := tx.Commit(ctx); err != nil {
		log.Printf("db tx commit error: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "failed to commit transaction"})
		return
	}

	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "created", "run_ids": runIDs})
}

// getRunHandler fetches a run by ID and resolves S3 byte-range refs for inputs/outputs/metadata.
func (s *Server) getRunHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	ctx := r.Context()

	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "id must be a valid UUID"})
		return
	}

	var (
		outID       uuid.UUID
		traceID     uuid.UUID
		name        string
		inputsRef   string
		outputsRef  string
		metadataRef string
	)
	conn, err := s.db.Acquire(ctx)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "failed to acquire database connection"})
		return
	}
	defer conn.Release()
	err = conn.QueryRow(ctx,
		`SELECT id, trace_id, name, COALESCE(inputs, ''), COALESCE(outputs, ''), COALESCE(metadata, '')
         FROM runs WHERE id = $1`, id,
	).Scan(&outID, &traceID, &name, &inputsRef, &outputsRef, &metadataRef)
	if err != nil {
		// Not found or other error
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("Run with ID %s not found", idStr)})
		return
	}

	var (
		inputs, outputs, metadata map[string]any
		wg                        sync.WaitGroup
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		inputs = s.fetchFromS3(ctx, inputsRef)
	}()
	go func() {
		defer wg.Done()
		outputs = s.fetchFromS3(ctx, outputsRef)
	}()
	go func() {
		defer wg.Done()
		metadata = s.fetchFromS3(ctx, metadataRef)
	}()
	wg.Wait()

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":       outID.String(),
		"trace_id": traceID.String(),
		"name":     name,
		"inputs":   inputs,
		"outputs":  outputs,
		"metadata": metadata,
	})
}

// parseS3Ref parses refs like s3://bucket/key#start:end/field
func (s *Server) parseS3Ref(ref string) (bucket, key string, start, end int, ok bool) {
	if ref == "" || !strings.HasPrefix(ref, "s3://") {
		return "", "", 0, 0, false
	}
	rest := strings.TrimPrefix(ref, "s3://")
	slash := strings.IndexByte(rest, '/')
	if slash == -1 {
		return "", "", 0, 0, false
	}
	bucket = rest[:slash]
	keyAndFrag := rest[slash+1:]
	key = keyAndFrag
	if i := strings.IndexByte(keyAndFrag, '#'); i != -1 {
		key = keyAndFrag[:i]
		frag := keyAndFrag[i+1:]
		// frag is like start:end/field
		if j := strings.IndexByte(frag, '/'); j != -1 {
			offsets := frag[:j]
			parts := strings.Split(offsets, ":")
			if len(parts) == 2 {
				s0, e0 := parts[0], parts[1]
				st, err1 := strconv.Atoi(s0)
				en, err2 := strconv.Atoi(e0)
				if err1 == nil && err2 == nil {
					start, end, ok = st, en, true
				}
			}
		}
	}
	return
}

// fetchFromS3 retrieves the JSON fragment using byte range, returns empty object on errors.
func (s *Server) fetchFromS3(ctx context.Context, ref string) map[string]any {
	bucket, key, start, end, ok := s.parseS3Ref(ref)
	if !ok || bucket == "" || key == "" || end <= start {
		return map[string]any{}
	}
	rng := fmt.Sprintf("bytes=%d-%d", start, end-1)
	out, err := s.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Range:  aws.String(rng),
	})
	if err != nil {
		log.Printf("s3 GetObject error: %v", err)
		return map[string]any{}
	}
	defer out.Body.Close()
	b, err := io.ReadAll(out.Body)
	if err != nil {
		return map[string]any{}
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		return map[string]any{}
	}
	return v
}

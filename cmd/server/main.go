package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/goccy/go-json"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
	ID       *string         `json:"id"`
	TraceID  string          `json:"trace_id"`
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

// copyBufPool provides reusable fixed-size buffers for io.CopyBuffer during streaming.
var copyBufPool = sync.Pool{New: func() any { b := make([]byte, 32*1024); return &b }}

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
	var runs []runJSON
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
	defer bufferPool.Put(buf)

	// Optional pre-grow: heuristic total size (tune factor)
	var est int
	for _, rj := range runs {
		est += len(rj.Inputs) + len(rj.Outputs) + len(rj.Metadata) + 256
	}
	if est > 0 && est < 64*1024*1024 {
		buf.Grow(est)
	}

	buf.WriteByte('[')
	offs := make([]runOffsets, 0, len(runs))

	quoteBuf := make([]byte, 0, 128)

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

		if i > 0 {
			buf.WriteByte(',')
		}

		buf.WriteString(`{"id":"`)
		buf.WriteString(id.String())
		buf.WriteString(`","trace_id":"`)
		buf.WriteString(traceID.String())
		buf.WriteString(`","name":`)

		quoteBuf = strconv.AppendQuote(quoteBuf[:0], in.Name)
		buf.Write(quoteBuf)

		// inputs
		buf.WriteString(`,"inputs":`)
		inputsStart := buf.Len()
		if len(in.Inputs) == 0 {
			buf.WriteString(`{}`)
		} else {
			// RawMessage: write directly (must be valid JSON)
			buf.Write(in.Inputs)
		}
		inputsEnd := buf.Len()

		// outputs
		buf.WriteString(`,"outputs":`)
		outputsStart := buf.Len()
		if len(in.Outputs) == 0 {
			buf.WriteString(`{}`)
		} else {
			buf.Write(in.Outputs)
		}
		outputsEnd := buf.Len()

		// metadata
		buf.WriteString(`,"metadata":`)
		metadataStart := buf.Len()
		if len(in.Metadata) == 0 {
			buf.WriteString(`{}`)
		} else {
			buf.Write(in.Metadata)
		}
		metadataEnd := buf.Len()

		buf.WriteByte('}')

		offs = append(offs, runOffsets{
			id:          id,
			traceID:     traceID,
			name:        in.Name,
			inputsRef:   fmt.Sprintf("s3://%s/%s#%d:%d/inputs", s.cfg.S3BucketName, objectKey, inputsStart, inputsEnd),
			outputsRef:  fmt.Sprintf("s3://%s/%s#%d:%d/outputs", s.cfg.S3BucketName, objectKey, outputsStart, outputsEnd),
			metadataRef: fmt.Sprintf("s3://%s/%s#%d:%d/metadata", s.cfg.S3BucketName, objectKey, metadataStart, metadataEnd),
		})
	}
	buf.WriteByte(']')
	// Return buffer to pool after use
	bufReader := bytes.NewReader(buf.Bytes())

	errCh := make(chan error, 2)
	runIDsCh := make(chan []string, 1)

	// S3 upload goroutine
	go func() {
		_, err := s.s3.PutObject(ctx, &s3.PutObjectInput{
			Bucket:      aws.String(s.cfg.S3BucketName),
			Key:         aws.String(objectKey),
			Body:        bufReader,
			ContentType: aws.String("application/json"),
		})
		errCh <- err
	}()

	// DB batch insert goroutine
	go func() {
		conn, err := s.db.Acquire(ctx)
		if err != nil {
			errCh <- err
			runIDsCh <- nil
			return
		}
		defer conn.Release()

		rows := make([][]any, 0, len(offs))
		runIDs := make([]string, 0, len(offs))
		for _, ro := range offs {
			rows = append(rows, []any{ro.id, ro.traceID, ro.name, ro.inputsRef, ro.outputsRef, ro.metadataRef})
			runIDs = append(runIDs, ro.id.String())
		}

		_, err = conn.CopyFrom(
			ctx,
			pgx.Identifier{"runs"},
			[]string{"id", "trace_id", "name", "inputs", "outputs", "metadata"},
			pgx.CopyFromRows(rows),
		)
		if err != nil {
			errCh <- err
			runIDsCh <- nil
			return
		}

		errCh <- nil
		runIDsCh <- runIDs
	}()

	var s3Err, dbErr error
	var runIDs []string
	for i := 0; i < 2; i++ {
		err := <-errCh
		if err != nil {
			if s3Err == nil {
				s3Err = err
			} else {
				dbErr = err
			}
		}
	}
	if dbErr == nil && s3Err == nil {
		runIDs = <-runIDsCh
	}

	if s3Err != nil || dbErr != nil {
		w.WriteHeader(http.StatusInternalServerError)
		msg := map[string]string{"error": ""}
		if s3Err != nil {
			msg["error"] += "S3 upload failed: " + s3Err.Error() + ". "
		}
		if dbErr != nil {
			msg["error"] += "DB insert failed: " + dbErr.Error()
		}
		_ = json.NewEncoder(w).Encode(msg)
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

	conn, err := s.db.Acquire(ctx)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "failed to acquire database connection"})
		return
	}
	defer conn.Release()

	var (
		outID       uuid.UUID
		traceID     uuid.UUID
		name        string
		inputsRef   string
		outputsRef  string
		metadataRef string
	)
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

	fields := []struct {
		key string
		ref string
	}{
		{"inputs", inputsRef},
		{"outputs", outputsRef},
		{"metadata", metadataRef},
	}
	type stream struct {
		key   string
		ref   string
		body  io.ReadCloser
		errCh <-chan error
	}
	streams := make([]stream, 0, len(fields))
	for _, f := range fields {
		rc, errCh := s.openS3RangePipe(ctx, f.ref)
		streams = append(streams, stream{key: f.key, ref: f.ref, body: rc, errCh: errCh})
	}

	w.WriteHeader(http.StatusOK)

	writeField := func(prefix string, st stream) {
		_, _ = w.Write([]byte(prefix)) // static JSON
		if st.body == nil {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		bufPtr := copyBufPool.Get().(*[]byte)
		copyBuf := *bufPtr
		_, copyErr := io.CopyBuffer(w, st.body, copyBuf)
		copyBufPool.Put(bufPtr)
		closeErr := st.body.Close()
		err := <-st.errCh
		if copyErr != nil || closeErr != nil || err != nil {
			log.Printf("stream field %s errors: copy=%v close=%v fetch=%v", st.key, copyErr, closeErr, err)
			// fallback empty object if error (optional)
			_, _ = w.Write([]byte(`{}`))
			return
		}
	}

	_, _ = w.Write([]byte(`{"id":"` + outID.String() + `","trace_id":"` + traceID.String() + `","name":`))
	nameBuf, _ := json.Marshal(name)
	_, _ = w.Write(nameBuf)

	writeField(`,"inputs":`, streams[0])
	writeField(`,"outputs":`, streams[1])
	writeField(`,"metadata":`, streams[2])
	_, _ = w.Write([]byte(`}`))
}

// parseS3Ref parses refs like s3://bucket/key#start:end/field
func (s *Server) parseS3Ref(ref string) (bucket, key string, start, end int, ok bool) {
	if ref == "" || !strings.HasPrefix(ref, "s3://") {
		return "", "", 0, 0, false
	}
	rest := ref[5:] // skip "s3://"
	slash := strings.IndexByte(rest, '/')
	if slash == -1 {
		return "", "", 0, 0, false
	}
	bucket = rest[:slash]
	keyAndFrag := rest[slash+1:]
	key = keyAndFrag
	if hash := strings.IndexByte(keyAndFrag, '#'); hash != -1 {
		key = keyAndFrag[:hash]
		frag := keyAndFrag[hash+1:]
		// frag is like start:end/field
		if slash2 := strings.IndexByte(frag, '/'); slash2 != -1 {
			offsets := frag[:slash2]
			parts := strings.SplitN(offsets, ":", 2)
			if len(parts) == 2 {
				st, err1 := strconv.Atoi(parts[0])
				en, err2 := strconv.Atoi(parts[1])
				if err1 == nil && err2 == nil {
					start, end, ok = st, en, true
				}
			}
		}
	}
	return
}

// openS3RangePipe returns a ReadCloser that streams the range specified by the ref using an io.Pipe.
// The returned error channel yields the terminal error (if any) after the copy completes.
func (s *Server) openS3RangePipe(ctx context.Context, ref string) (io.ReadCloser, <-chan error) {
	bucket, key, start, end, ok := s.parseS3Ref(ref)
	if !ok || bucket == "" || key == "" || end <= start {
		return nil, make(chan error, 1) // empty errCh
	}
	rng := fmt.Sprintf("bytes=%d-%d", start, end-1)
	pr, pw := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		defer close(errCh)
		out, err := s.s3.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
			Range:  aws.String(rng),
		})
		if err != nil {
			pw.CloseWithError(err)
			errCh <- err
			return
		}
		// Ensure body closed
		defer out.Body.Close()
		bufPtr := copyBufPool.Get().(*[]byte)
		copyBuf := *bufPtr
		_, copyErr := io.CopyBuffer(pw, out.Body, copyBuf)
		copyBufPool.Put(bufPtr)
		if copyErr != nil {
			pw.CloseWithError(copyErr)
			errCh <- copyErr
			return
		}
		pw.Close()
		errCh <- nil
	}()
	return pr, errCh
}

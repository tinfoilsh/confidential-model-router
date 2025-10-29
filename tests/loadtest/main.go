package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// CLI tunables let us dial up concurrency or change prompts when we want to push the router harder.
var (
	baseURL    = flag.String("base-url", "http://localhost:8089/v1", "router base URL")
	modelName  = flag.String("model", "deepseek-v31-terminus", "model identifier the router expects")
	duration   = flag.Duration("duration", 15*time.Minute, "total time to keep the load running")
	workers    = flag.Int("workers", 300, "number of concurrent request workers")
	maxTokens  = flag.Int("max-tokens", 2048, "max tokens per completion request")
	systemMsg  = flag.String("system", "You are a helpful assistant.", "system prompt used for each request")
	userPrompt = flag.String("prompt", "Describe quantum key distribution in detail.", "user prompt used for each request")
	retryMax   = flag.Int("retry-max", 5, "max number of retries per request on HTTP 429")
	rampUp     = flag.Duration("ramp-up", 60*time.Second, "time to ramp up all workers (0 for immediate start)")
)

func main() {
	flag.Parse()

	apiKey := mustEnv("OPENAI_API_KEY")

	// Use a shared http.Client so the SDK reuses connections and each retry is still bounded by a hard timeout.
	httpClient := &http.Client{
		Timeout: 10 * time.Minute,
	}

	client := openai.NewClient(
		option.WithAPIKey(apiKey),
		option.WithBaseURL(*baseURL),
		option.WithHTTPClient(httpClient),
	)

	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()

	model := openai.ChatModel(*modelName)
	systemPrompt := *systemMsg
	baseUserPrompt := *userPrompt
	maxCompletionTokens := int64(*maxTokens)

	// Allow Ctrl+C to stop the test early without waiting for the timeout.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-stop:
			log.Println("received interrupt signal, stopping load...")
			cancel()
		case <-ctx.Done():
		}
	}()

	var (
		wg         sync.WaitGroup
		mu         sync.Mutex
		durations  []float64
		successes  atomic.Int64
		rejections atomic.Int64
		failures   atomic.Int64
	)

	// Track when load test actually starts accepting traffic
	var rampStartTime atomic.Value
	rampStartTime.Store(time.Time{})

	// Spin up N workers; each repeatedly sends chat completions while recording latency.
	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			// Stagger startup to simulate realistic traffic ramp
			if *rampUp > 0 {
				// Distribute worker starts evenly across ramp-up period
				stagger := time.Duration(id) * (*rampUp) / time.Duration(*workers)

				// Record the first worker start time for logging
				if id == 0 {
					log.Printf("Starting ramp-up: %d workers over %s", *workers, *rampUp)
					rampStartTime.Store(time.Now())
				}

				select {
				case <-time.After(stagger):
				case <-ctx.Done():
					return
				}

				if id == *workers-1 {
					elapsed := time.Since(rampStartTime.Load().(time.Time))
					log.Printf("Ramp-up complete: all %d workers started in %s", *workers, elapsed)
				}
			}

			for {
				if ctx.Err() != nil {
					return
				}

				start := time.Now()
				params := buildParams(model, systemPrompt, baseUserPrompt, maxCompletionTokens)
				err := issueWithRetries(ctx, client, params, *retryMax, &rejections)
				if err != nil {
					if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						return
					}
					// Non-429 failure means the router or client surfaced a real error—track it separately.
					failures.Add(1)
					log.Printf("[worker %d] request failed: %v", id, err)
					select {
					case <-time.After(2 * time.Second):
					case <-ctx.Done():
						return
					}
					continue
				}

				latency := time.Since(start).Seconds()
				successes.Add(1)
				mu.Lock()
				durations = append(durations, latency)
				mu.Unlock()
			}
		}(i)
	}

	wg.Wait()

	mu.Lock()
	snapshot := append([]float64(nil), durations...)
	mu.Unlock()
	sort.Float64s(snapshot)

	p50 := percentile(snapshot, 0.50)
	p90 := percentile(snapshot, 0.90)
	p99 := percentile(snapshot, 0.99)

	rejected := rejections.Load()
	log.Printf("completed=%d rejected=%d errors=%d p50=%.2fs p90=%.2fs p99=%.2fs",
		successes.Load(), rejected, failures.Load(), p50, p90, p99)
	if rejected > 0 {
		log.Printf("ratio: %.2f%% of attempts received 429 Retry-After responses", float64(rejected)/float64(successes.Load()+rejected+failures.Load())*100)
	}
}

// issueWithRetries wraps the SDK call so we can enforce retry budgets when the router returns 429.
// We intentionally rely on the SDK's default retry loop first, then add our own guard so the test
// fails loudly if Retry-After keeps us stalled beyond the configured attempts.
func issueWithRetries(ctx context.Context, client openai.Client, params openai.ChatCompletionNewParams, retryMax int, rejections *atomic.Int64) error {
	attempt := 0
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		reqCtx, cancel := context.WithTimeout(ctx, 12*time.Minute)
		_, err := client.Chat.Completions.New(reqCtx, params)
		cancel()

		if err == nil {
			return nil
		}

		var apiErr *openai.Error
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusTooManyRequests {
			rejections.Add(1)
			if attempt >= retryMax {
				return fmt.Errorf("exceeded %d retry attempts after HTTP 429: %w", retryMax, err)
			}
			attempt++
			wait := retryDelay(apiErr.Response)
			if wait <= 0 {
				wait = 10 * time.Second
			}
			select {
			case <-time.After(wait):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		return err
	}
}

// retryDelay parses standard Retry-After formats so we honor router guidance when backing off.
func retryDelay(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}

	value := resp.Header.Get("Retry-After")
	if value == "" {
		return 0
	}

	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil {
		delay := time.Until(date)
		if delay > 0 {
			return delay
		}
	}
	if duration, err := time.ParseDuration(value); err == nil {
		return duration
	}

	return 0
}

func buildParams(model openai.ChatModel, systemPrompt, baseUserPrompt string, maxTokens int64) openai.ChatCompletionNewParams {
	requestID := randomRequestID()
	userContent := fmt.Sprintf("request-id: %s\n\n%s", requestID, baseUserPrompt)
	return openai.ChatCompletionNewParams{
		Model: model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(systemPrompt),
			openai.UserMessage(userContent),
		},
		MaxTokens: openai.Int(maxTokens),
	}
}

func percentile(samples []float64, pct float64) float64 {
	if len(samples) == 0 {
		return math.NaN()
	}
	if pct <= 0 {
		return samples[0]
	}
	if pct >= 1 {
		return samples[len(samples)-1]
	}
	index := int(math.Ceil(pct*float64(len(samples)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(samples) {
		index = len(samples) - 1
	}
	return samples[index]
}

func mustEnv(key string) string {
	value := os.Getenv(key)
	if value == "" {
		log.Fatalf("missing %s environment variable", key)
	}
	return value
}

func randomRequestID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", buf)
}

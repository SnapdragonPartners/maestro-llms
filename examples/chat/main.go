// Command chat is a small, runs-locally web demo for maestro-llms. It
// reads provider credentials from the environment, exposes one chat
// model per reachable provider in a dropdown, and lets the user trade
// messages with the selected model.
//
// Run:
//
//	# Set whichever provider keys you have available, then:
//	cd examples/chat && go run .
//	# open http://localhost:8765
//
// The demo is intentionally small and stays in this examples/ module so
// it does not pollute the toolkit's import surface. It does, however,
// exercise the production-shape wiring (RecommendedChat middleware) so
// reading it gives a developer a head start on what their own service
// should look like.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", ":8765", "address to listen on (env PORT overrides if set)")
	flag.Parse()
	if p := os.Getenv("PORT"); p != "" {
		*addr = net.JoinHostPort("", p)
	}

	logger := log.New(os.Stderr, "chat-demo: ", log.LstdFlags|log.Lmsgprefix)

	// Discover what providers are reachable in this environment. We do
	// this once at startup so the dropdown is ready when the user loads
	// the page; live re-detection on every refresh would be wasted work
	// for a single-user demo.
	//
	// We deliberately pass an UNBOUNDED context here. Each provider
	// detector applies its own per-call deadline inside providers.go,
	// so a slow or dead provider can't eat the budget for the others.
	// Total startup is bounded by sum-of-per-provider-timeouts in the
	// worst case (every provider configured but unreachable); typically
	// well under a second.
	options := detectAvailable(context.Background(), logger.Printf)
	if len(options) == 0 {
		logger.Println("no providers detected. Set at least one of:")
		logger.Println("  ANTHROPIC_API_KEY  (or MAESTRO_ANTHROPIC_API_KEY)")
		logger.Println("  OPENAI_API_KEY")
		logger.Println("  GEMINI_API_KEY     (or GOOGLE_GENAI_API_KEY / GOOGLE_API_KEY)")
		logger.Println("  OLLAMA_HOST        (defaults to http://localhost:11434)")
		logger.Println("  MAESTRO_VLLM       (full base URL of a vLLM instance)")
		os.Exit(1)
	}
	logger.Printf("detected %d provider option(s):", len(options))
	for _, opt := range options {
		logger.Printf("  - %s", opt.Label)
	}

	mux := newServer(options, logger)
	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	idle := make(chan struct{})
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		logger.Println("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Printf("shutdown: %v", err)
		}
		close(idle)
	}()

	logger.Printf("listening on http://localhost%s — open in your browser", *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatalf("listen: %v", err)
	}
	<-idle
	fmt.Fprintln(os.Stderr, "chat-demo: bye.")
}

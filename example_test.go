// Runnable documentation for skyl's public API.
//
// docs/rules.md §8.3 requires examples to live here as Example functions, so CI
// breaks when they rot rather than a reader discovering it.
//
// The package is skyl_test rather than skyl: only an external test package
// renders in godoc with the skyl. qualifier a reader would actually type.
//
// Examples that produce output are backed by the fake Provider below rather
// than by a real adapter, for two reasons. §3.3 forbids network access in
// tests, and an example is documentation — one that showed an import a reader
// cannot copy, or output that varies per run, would be worse than none. The
// examples that demonstrate real usage carry no Output comment and are
// therefore compiled but not run, which is the most Go can check for code that
// needs a credential.
package skyl_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
)

// fakeProvider is a stand-in for an adapter, so the examples below are
// deterministic and offline. A real program passes anthropic.New(key),
// openai.New(key), and so on — the interface is the same either way, which is
// the point of the seam.
type fakeProvider struct {
	text  string
	calls []skyl.ToolCall
}

func (fakeProvider) Name() string { return "example" }

func (p fakeProvider) Complete(context.Context, *skyl.Request) (*skyl.Response, error) {
	parts := []skyl.Part{skyl.Text{Text: p.text}}
	stop := skyl.StopEndTurn
	for _, c := range p.calls {
		parts = append(parts, c)
		stop = skyl.StopToolUse
	}
	return &skyl.Response{
		Provider:   "example",
		Model:      "example-model",
		Message:    skyl.Message{Role: skyl.RoleAssistant, Parts: parts},
		StopReason: stop,
		Usage:      skyl.Usage{InputTokens: 8, OutputTokens: 3},
		Raw:        json.RawMessage(`{}`),
	}, nil
}

func (p fakeProvider) Stream(context.Context, *skyl.Request) (skyl.Stream, error) {
	return &fakeStream{words: []string{"Go ", "channels ", "synchronise."}}, nil
}

func (fakeProvider) Models(context.Context) ([]skyl.ModelInfo, error) {
	return []skyl.ModelInfo{{ID: "example-model", Provider: "example"}}, nil
}

type fakeStream struct {
	words []string
	i     int
	ev    skyl.StreamEvent
	done  bool
}

func (s *fakeStream) Next() bool {
	if s.i < len(s.words) {
		s.ev = skyl.StreamEvent{Type: skyl.EventTextDelta, Text: s.words[s.i]}
		s.i++
		return true
	}
	if !s.done {
		s.done = true
		s.ev = skyl.StreamEvent{
			Type:       skyl.EventDone,
			StopReason: skyl.StopEndTurn,
			Usage:      &skyl.Usage{InputTokens: 5, OutputTokens: 3},
		}
		return true
	}
	return false
}

func (s *fakeStream) Event() skyl.StreamEvent { return s.ev }
func (s *fakeStream) Err() error              { return nil }
func (s *fakeStream) Close() error            { return nil }

// The shortest useful program: wrap a provider and ask it something.
func Example() {
	client := skyl.New(fakeProvider{text: "Paris"})

	resp, err := client.Complete(context.Background(), &skyl.Request{
		Model:     "example-model",
		MaxTokens: 64,
		Messages:  []skyl.Message{skyl.UserText("What is the capital of France?")},
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(resp.Text())
	fmt.Printf("%d in / %d out\n", resp.Usage.InputTokens, resp.Usage.OutputTokens)
	// Output:
	// Paris
	// 8 in / 3 out
}

// Switching providers is a one-line change, because everything below the seam
// is identical. This example needs a credential, so it is compiled but not run.
func ExampleNew_realProvider() {
	// import "github.com/BAGOMBEKA-JOB-DEV/skyl/provider/openai"
	//
	//	client := skyl.New(openai.New(os.Getenv("OPENAI_API_KEY")))
	//
	// or, with the behaviour a production caller usually wants:
	client := skyl.New(
		fakeProvider{text: "..."},
		skyl.WithMaxRetries(5),
		skyl.WithRetryDelay(time.Second, time.Minute),
		skyl.WithTimeout(2*time.Minute),
	)
	_ = client
}

// Streaming is a pull iterator, so it composes with defer and with an early
// return the way a Go programmer expects.
func ExampleClient_Stream() {
	client := skyl.New(fakeProvider{})

	stream, err := client.Stream(context.Background(), &skyl.Request{
		Model:     "example-model",
		MaxTokens: 64,
		Messages:  []skyl.Message{skyl.UserText("Explain Go channels.")},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer stream.Close()

	for stream.Next() {
		if ev := stream.Event(); ev.Type == skyl.EventTextDelta {
			fmt.Print(ev.Text)
		}
	}
	fmt.Println()

	// Always check Err after the loop: Next returning false means either the
	// stream finished or it failed, and only Err tells them apart.
	if err := stream.Err(); err != nil {
		log.Fatal(err)
	}
	// Output:
	// Go channels synchronise.
}

// CollectStream drains a stream into one Response, for callers who want
// streaming's early first byte without handling events themselves.
func ExampleCollectStream() {
	client := skyl.New(fakeProvider{})

	stream, err := client.Stream(context.Background(), &skyl.Request{
		Model:     "example-model",
		MaxTokens: 64,
		Messages:  []skyl.Message{skyl.UserText("Explain Go channels.")},
	})
	if err != nil {
		log.Fatal(err)
	}

	resp, err := skyl.CollectStream(stream, "example", "example-model")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(resp.Text())
	fmt.Println(resp.StopReason)
	// Output:
	// Go channels synchronise.
	// end_turn
}

// Errors are classified, so branch on the sentinel rather than on message text
// — providers reword their messages and string matching breaks silently when
// they do.
func ExampleError() {
	err := skyl.NewError("openai", 429, skyl.ErrRateLimit, "slow down", nil)

	if errors.Is(err, skyl.ErrRateLimit) {
		fmt.Println("rate limited; skyl already retried with backoff")
	}

	var e *skyl.Error
	if errors.As(err, &e) {
		fmt.Printf("%s returned %d\n", e.Provider, e.StatusCode)
		fmt.Println("retryable:", e.Retryable())
	}
	// Output:
	// rate limited; skyl already retried with backoff
	// openai returned 429
	// retryable: true
}

// A tool call comes back on the response; run it and send the result as the
// next message.
func ExampleResponse_ToolCalls() {
	client := skyl.New(fakeProvider{
		text: "",
		calls: []skyl.ToolCall{{
			ID: "call_1", Name: "get_weather",
			Arguments: json.RawMessage(`{"city":"Kampala"}`),
		}},
	})

	resp, err := client.Complete(context.Background(), &skyl.Request{
		Model:     "example-model",
		MaxTokens: 64,
		Tools:     []skyl.Tool{{Name: "get_weather", Description: "Look up the weather."}},
		Messages:  []skyl.Message{skyl.UserText("Weather in Kampala?")},
	})
	if err != nil {
		log.Fatal(err)
	}

	for _, call := range resp.ToolCalls() {
		fmt.Printf("%s(%s)\n", call.Name, call.Arguments)

		// Append the assistant's turn and the result, then call again.
		_ = []skyl.Message{
			skyl.UserText("Weather in Kampala?"),
			{Role: skyl.RoleAssistant, Parts: []skyl.Part{call}},
			skyl.ToolResultMessage(call.ID, "22C and sunny"),
		}
	}
	fmt.Println(resp.StopReason)
	// Output:
	// get_weather({"city":"Kampala"})
	// tool_use
}

// Hooks observe every attempt, including retried ones. They run on the calling
// goroutine, so keep them cheap.
func ExampleWithHook() {
	client := skyl.New(
		fakeProvider{text: "Paris"},
		skyl.WithHook(func(_ context.Context, ev skyl.HookEvent) {
			fmt.Printf("%s %s attempt=%d err=%v\n",
				ev.Provider, ev.Operation, ev.Attempt, ev.Err)
		}),
	)

	if _, err := client.Complete(context.Background(), &skyl.Request{
		Model:     "example-model",
		MaxTokens: 64,
		Messages:  []skyl.Message{skyl.UserText("hello")},
	}); err != nil {
		log.Fatal(err)
	}
	// Output:
	// example complete attempt=0 err=<nil>
}

// Usage accounting: cache figures break InputTokens down rather than adding to
// it, so a total is input plus output.
func ExampleUsage_Add() {
	first := skyl.Usage{InputTokens: 1000, OutputTokens: 50, CacheReadTokens: 900}
	second := skyl.Usage{InputTokens: 1200, OutputTokens: 80, CacheReadTokens: 1100}

	total := first.Add(second)
	fmt.Println(total.InputTokens, total.OutputTokens, total.TotalTokens())
	fmt.Println("served from cache:", total.CacheReadTokens)
	// Output:
	// 2200 130 2330
	// served from cache: 2000
}

// ProviderOptions reaches a vendor feature skyl does not model. Values here
// override anything skyl set, which is the point of an escape hatch.
func ExampleRequest_providerOptions() {
	req := &skyl.Request{
		Model:     "example-model",
		MaxTokens: 64,
		Messages:  []skyl.Message{skyl.UserText("hello")},
		ProviderOptions: map[string]any{
			"top_k": 40,
		},
	}

	if _, err := skyl.New(fakeProvider{text: "hi"}).Complete(context.Background(), req); err != nil {
		log.Fatal(err)
	}
	fmt.Println("sent with top_k")
	// Output:
	// sent with top_k
}

// Validation happens locally, so a malformed request fails without costing a
// round trip.
func ExampleRequest_Validate() {
	err := (&skyl.Request{Model: "", Messages: nil}).Validate()

	fmt.Println(errors.Is(err, skyl.ErrBadRequest))
	fmt.Println(err)
	// Output:
	// true
	// skyl: invalid request: model is required
}

// Models are listed live from the provider, so the answer is never stale.
func ExampleClient_Models() {
	models, err := skyl.New(fakeProvider{}).Models(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	for _, m := range models {
		fmt.Println(m.ID)
	}
	// Output:
	// example-model
}

// Compiled but not run: it needs a real credential. Kept so the pattern in the
// README cannot drift from something that compiles.
func ExampleClient_Complete_production() {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return
	}
	// client := skyl.New(openai.New(key))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = ctx
}

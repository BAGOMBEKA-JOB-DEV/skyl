// Package provider_test runs the shared adapter contract against every
// adapter that lives in the core module.
//
// Keeping them in one file means a rule added to the contract is enforced on
// all of them at once, and a regression in one adapter cannot hide behind
// another adapter's tests.
package provider_test

import (
	"testing"

	"github.com/BAGOMBEKA-JOB-DEV/skyl"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/internal/providertest"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/provider/gemini"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/provider/openai"
	"github.com/BAGOMBEKA-JOB-DEV/skyl/provider/openaicompat"
)

// The OpenAI wire format, shared by provider/openai and provider/openaicompat.
const (
	oaiSuccess = `{
		"id": "chatcmpl-1",
		"model": "served-model-0613",
		"choices": [{"message": {"role": "assistant", "content": "hi there"},
		             "finish_reason": "stop"}],
		"usage": {"prompt_tokens": 9, "completion_tokens": 3}
	}`
	oaiError = `{"error": {"message": "something went wrong", "type": "invalid_request_error"}}`
)

var oaiStream = []string{
	`{"choices":[{"delta":{"content":"a"}}]}`,
	`{"choices":[{"delta":{"content":"b"}}]}`,
	`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	`[DONE]`,
}

func TestOpenAIContract(t *testing.T) {
	t.Parallel()

	providertest.Suite{
		Name:   "openai",
		APIKey: "sk-test-openai-key",
		New: func(baseURL string) skyl.Provider {
			return openai.New("sk-test-openai-key", openai.WithBaseURL(baseURL))
		},
		SuccessBody:  oaiSuccess,
		WantText:     "hi there",
		WantModel:    "served-model-0613",
		ErrorBody:    oaiError,
		StreamFrames: oaiStream,
	}.Run(t)
}

func TestOpenAICompatContract(t *testing.T) {
	t.Parallel()

	providertest.Suite{
		Name:   "openaicompat",
		APIKey: "sk-test-compat-key",
		New: func(baseURL string) skyl.Provider {
			return openaicompat.New(
				openaicompat.WithBaseURL(baseURL),
				openaicompat.WithAPIKey("sk-test-compat-key"),
				openaicompat.WithName("compat-under-test"),
			)
		},
		SuccessBody:  oaiSuccess,
		WantText:     "hi there",
		WantModel:    "served-model-0613",
		ErrorBody:    oaiError,
		StreamFrames: oaiStream,
	}.Run(t)
}

func TestGeminiContract(t *testing.T) {
	t.Parallel()

	providertest.Suite{
		Name:   "gemini",
		APIKey: "test-gemini-key",
		New: func(baseURL string) skyl.Provider {
			return gemini.New("test-gemini-key", gemini.WithBaseURL(baseURL))
		},
		SuccessBody: `{
			"candidates": [{"content": {"role": "model", "parts": [{"text": "hi there"}]},
			                "finishReason": "STOP"}],
			"usageMetadata": {"promptTokenCount": 9, "candidatesTokenCount": 3},
			"modelVersion": "served-model-0613"
		}`,
		WantText:  "hi there",
		WantModel: "served-model-0613",
		ErrorBody: `{"error": {"code": 400, "message": "something went wrong", "status": "INVALID_ARGUMENT"}}`,
		StreamFrames: []string{
			`{"candidates":[{"content":{"parts":[{"text":"a"}]}}]}`,
			`{"candidates":[{"content":{"parts":[{"text":"b"}]}}]}`,
			`{"candidates":[{"content":{"parts":[]},"finishReason":"STOP"}]}`,
		},
	}.Run(t)
}

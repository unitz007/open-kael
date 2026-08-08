package openai

import "encoding/json"

type Response struct {
	Id                string      `json:"id"`
	Object            string      `json:"object"`
	Created           int         `json:"created"`
	Model             string      `json:"model"`
	Provider          string      `json:"provider"`
	SystemFingerprint interface{} `json:"system_fingerprint"`
	ServiceTier       interface{} `json:"service_tier"`
	Choices           []struct {
		Index              int         `json:"index"`
		Logprobs           interface{} `json:"logprobs"`
		FinishReason       string      `json:"finish_reason"`
		NativeFinishReason string      `json:"native_finish_reason"`
		Message            struct {
			Role      string      `json:"role"`
			Content   string      `json:"content"`
			Refusal   interface{} `json:"refusal"`
			Reasoning string      `json:"reasoning"`
			ToolCalls []struct {
				Type     string `json:"type"`
				Index    int    `json:"index"`
				Id       string `json:"id"`
				Function struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
			ReasoningDetails []struct {
				Type   string `json:"type"`
				Text   string `json:"text"`
				Format string `json:"format"`
				Index  int    `json:"index"`
			} `json:"reasoning_details"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens        int     `json:"prompt_tokens"`
		CompletionTokens    int     `json:"completion_tokens"`
		TotalTokens         int     `json:"total_tokens"`
		Cost                float64 `json:"cost"`
		IsByok              bool    `json:"is_byok"`
		PromptTokensDetails struct {
			CachedTokens     int `json:"cached_tokens"`
			CacheWriteTokens int `json:"cache_write_tokens"`
			AudioTokens      int `json:"audio_tokens"`
			VideoTokens      int `json:"video_tokens"`
		} `json:"prompt_tokens_details"`
		CostDetails struct {
			UpstreamInferenceCost            float64 `json:"upstream_inference_cost"`
			UpstreamInferencePromptCost      float64 `json:"upstream_inference_prompt_cost"`
			UpstreamInferenceCompletionsCost float64 `json:"upstream_inference_completions_cost"`
		} `json:"cost_details"`
		CompletionTokensDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
			ImageTokens     int `json:"image_tokens"`
			AudioTokens     int `json:"audio_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}

package autoclaw

// Probe measures upstream timing without exposing any credential or content.
// It is compiled into the gateway only for diagnostics; nothing here is on the
// inference path.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/inference"
)

// ProbeAccount is the minimum an authenticated chat request needs.
type ProbeAccount struct {
	ID          string
	AccessToken string
}

// ProbeReport is the diagnostic result. Every field is a count or a duration:
// no token, phone number, conversation id or response text ever appears here.
type ProbeReport struct {
	Model string `json:"model"`

	// HeaderMS is how long the upstream took to return response headers. If
	// this alone is huge, the upstream is queueing us before doing any work.
	HeaderMS int64 `json:"header_ttfb_ms"`
	// FirstFrameMS is the first SSE data frame of ANY kind.
	FirstFrameMS int64 `json:"first_frame_ms"`
	// FirstReasoningMS is the first delta carrying reasoning_content — what
	// the desktop client renders as "思考 Ns".
	FirstReasoningMS int64 `json:"first_reasoning_ms"`
	// FirstContentMS is the first delta carrying answer content.
	FirstContentMS int64 `json:"first_content_ms"`
	// EndMS is the whole request.
	EndMS int64 `json:"end_ms"`

	ReasoningFrames int `json:"reasoning_frames"`
	ContentFrames   int `json:"content_frames"`
	ContentChars    int `json:"content_chars"`

	UsagePrompt     int  `json:"usage_prompt_tokens"`
	UsageCompletion int  `json:"usage_completion_tokens"`
	UsageSeen       bool `json:"usage_seen"`

	HTTPStatus int    `json:"http_status"`
	ContentType string `json:"content_type"`
	ErrorKind  string `json:"error_kind,omitempty"`
}

// ProbeAccessToken reads one account's access token for the diagnostic call.
// The caller must not log the returned value.
func ProbeAccessToken(vault *accountvault.Vault, id string) (string, error) {
	_, plain, err := vault.Get(id)
	if err != nil {
		return "", err
	}
	defer wipe(plain)
	var s stored
	if json.Unmarshal(plain, &s) != nil {
		return "", errNoProbeProfile
	}
	return s.AccessToken, nil
}

var errNoProbeProfile = &probeError{"probe_profile_unreadable"}

type probeError struct{ code string }

func (e *probeError) Error() string { return e.code }

// Probe sends exactly one streaming chat request and records the timings of
// each phase. It never retries: the point is to measure one honest call.
func Probe(ctx context.Context, account ProbeAccount, model, prompt string) (ProbeReport, error) {
	report := ProbeReport{Model: model}
	started := time.Now()

	body, err := buildBody(model, []inference.Message{{Role: "user", Content: prompt}}, true)
	if err != nil {
		return report, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+apiBase+chatPath, bytes.NewReader(body))
	if err != nil {
		return report, err
	}
	setHeaders(req, account.AccessToken, within(model))

	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	report.HeaderMS = time.Since(started).Milliseconds()
	if err != nil {
		report.ErrorKind = "transport"
		return report, err
	}
	defer resp.Body.Close()
	report.HTTPStatus = resp.StatusCode
	report.ContentType = resp.Header.Get("Content-Type")
	if resp.StatusCode != http.StatusOK {
		report.ErrorKind = "http_" + strings.TrimSpace(http.StatusText(resp.StatusCode))
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = raw
		return report, nil
	}

	// Read manually so each phase gets its own timestamp.
	buf := make([]byte, 8192)
	pending := ""
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			now := time.Since(started).Milliseconds()
			pending += string(buf[:n])
			for {
				idx := strings.IndexByte(pending, '\n')
				if idx < 0 {
					break
				}
				line := strings.TrimSpace(pending[:idx])
				pending = pending[idx+1:]
				if !strings.HasPrefix(line, "data:") {
					continue
				}
				payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if payload == "" || payload == "[DONE]" {
					continue
				}
				if report.FirstFrameMS == 0 {
					report.FirstFrameMS = now
				}
				var chunk struct {
					Choices []struct {
						Delta struct {
							Content          string `json:"content"`
							ReasoningContent string `json:"reasoning_content"`
						} `json:"delta"`
					} `json:"choices"`
					Usage json.RawMessage `json:"usage"`
				}
				if json.Unmarshal([]byte(payload), &chunk) != nil {
					continue
				}
				if len(chunk.Usage) > 0 && string(chunk.Usage) != "null" {
					var u struct {
						PromptTokens     *int `json:"prompt_tokens"`
						CompletionTokens *int `json:"completion_tokens"`
					}
					if json.Unmarshal(chunk.Usage, &u) == nil {
						if u.PromptTokens != nil {
							report.UsagePrompt = *u.PromptTokens
						}
						if u.CompletionTokens != nil {
							report.UsageCompletion = *u.CompletionTokens
						}
						report.UsageSeen = true
					}
				}
				for _, c := range chunk.Choices {
					if c.Delta.ReasoningContent != "" {
						report.ReasoningFrames++
						if report.FirstReasoningMS == 0 {
							report.FirstReasoningMS = now
						}
					}
					if c.Delta.Content != "" {
						report.ContentFrames++
						report.ContentChars += len([]rune(c.Delta.Content))
						if report.FirstContentMS == 0 {
							report.FirstContentMS = now
						}
					}
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			report.ErrorKind = "stream"
			report.EndMS = time.Since(started).Milliseconds()
			return report, readErr
		}
	}
	report.EndMS = time.Since(started).Milliseconds()
	return report, nil
}

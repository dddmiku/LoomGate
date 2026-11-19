package doubao

// Probe is a read-only diagnostic. It sends one real work-mode request and
// reports only the response envelope: status, content type, event names and the
// first event's shape. It never returns the Cookie, any token, or message text.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/inference"
)

type ProbeReport struct {
	HTTPStatus     int            `json:"http_status"`
	ContentType    string         `json:"content_type"`
	BytesRead      int            `json:"bytes_read"`
	EventNames     map[string]int `json:"event_names"`
	FirstEventName string         `json:"first_event_name,omitempty"`
	TextChars      int            `json:"text_chars"`
	AckConversation bool          `json:"ack_conversation"`
	ErrorKind      string         `json:"error_kind,omitempty"`
	ErrorFingerprint string       `json:"error_fingerprint,omitempty"`
	ElapsedMS      int64          `json:"elapsed_ms"`
}

func Probe(ctx context.Context, vault *accountvault.Vault, accountID, prompt string) (ProbeReport, error) {
	p := &Provider{Vault: vault, Client: &http.Client{Timeout: 0}}
	s, err := p.session(accountID)
	if err != nil {
		return ProbeReport{}, err
	}
	m := resolve(defaultModel)
	body, err := buildBody(s, m, []inference.Message{{Role: "user", Content: prompt}})
	if err != nil {
		return ProbeReport{}, err
	}
	start := time.Now()
	resp, err := p.send(ctx, s, body)
	if err != nil {
		return ProbeReport{}, err
	}
	defer resp.Body.Close()

	report := ProbeReport{HTTPStatus: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), EventNames: map[string]int{}}
	if !strings.Contains(strings.ToLower(report.ContentType), "event-stream") {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		report.BytesRead = len(raw)
		report.ErrorKind = "non-sse"
		report.ErrorFingerprint = summarizeBody(raw)
		report.ElapsedMS = time.Since(start).Milliseconds()
		return report, nil
	}

	var text strings.Builder
	buf := make([]byte, 0, 64<<10)
	tmp := make([]byte, 16<<10)
	pending := ""
	for {
		n, readErr := resp.Body.Read(tmp)
		if n > 0 {
			report.BytesRead += n
			pending += string(tmp[:n])
			buf = buf[:0]
			for {
				idx, sep := frameEnd(pending)
				if idx < 0 {
					break
				}
				frame := pending[:idx]
				pending = pending[idx+sep:]
				ev, data := parseFrame(frame)
				if ev == "" {
					continue
				}
				report.EventNames[ev]++
				if report.FirstEventName == "" {
					report.FirstEventName = ev
				}
				switch ev {
				case "CHUNK_DELTA":
					var d struct {
						Text string `json:"text"`
					}
					if json.Unmarshal([]byte(data), &d) == nil {
						text.WriteString(d.Text)
					}
				case "STREAM_CHUNK":
					var d struct {
						PatchOp []struct {
							PatchValue struct {
								ContentBlock []struct {
									BlockType int `json:"block_type"`
									Content   struct {
										TextBlock *struct {
											Text string `json:"text"`
										} `json:"text_block"`
									} `json:"content"`
								} `json:"content_block"`
							} `json:"patch_value"`
						} `json:"patch_op"`
					}
					if json.Unmarshal([]byte(data), &d) == nil {
						for _, op := range d.PatchOp {
							for _, b := range op.PatchValue.ContentBlock {
								if b.BlockType == 10000 && b.Content.TextBlock != nil {
									text.WriteString(b.Content.TextBlock.Text)
								}
							}
						}
					}
				case "SSE_ACK":
					report.AckConversation = strings.Contains(data, `"conversation_id"`)
				}
			}
		}
		if readErr != nil {
			break
		}
	}
	report.TextChars = len([]rune(text.String()))
	report.ElapsedMS = time.Since(start).Milliseconds()
	names := make([]string, 0, len(report.EventNames))
	for name := range report.EventNames {
		names = append(names, name)
	}
	sort.Strings(names)
	return report, nil
}

package upstream

import (
	"math/rand"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// Keep output-budget and provisional usage estimates identical while changing
// the scanner. Include malformed UTF-8 and data URIs, not only normal prose.
func TestPromptEstimateMatchesPriorBudgetForMixedInput(t *testing.T) {
	inputs := [][]byte{nil, []byte("ASCII request 123"), []byte("中文かなカナ한글 αé😀\n"),
		[]byte{0xff, 0xfe, 0xc0, 'd', 0, 'x'},
		[]byte(`{"image":"data:image/png;base64,AAAA","text":"你好"}`),
		[]byte(`{"text":"data: without base64; data: repeated"}`),
		[]byte(strings.Repeat("data:", 10000)),
	}
	rng := rand.New(rand.NewSource(17))
	fragments := []string{"a", "中", "あ", "한", "é", "😀", "data:", `"`, ";base64,", string([]byte{0xff})}
	for range 100 {
		var sample strings.Builder
		for range 100 {
			sample.WriteString(fragments[rng.Intn(len(fragments))])
		}
		inputs = append(inputs, []byte(sample.String()))
	}
	for index, body := range inputs {
		var cjk, other int64
		var scan dataURIScan
		for i := 0; i < len(body); {
			if body[i] == 'd' {
				if skip := scan.skip(body, i); skip > 0 {
					i += skip
					continue
				}
			}
			r, size := utf8.DecodeRune(body[i:])
			if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) {
				cjk++
			} else {
				other++
			}
			i += size
		}
		wantPrompt := (cjk*10+17)/18 + (other+3)/4
		if got := estimatePromptTokens(body); got != wantPrompt {
			t.Fatalf("case %d prompt estimate=%d want %d", index, got, wantPrompt)
		}
		for _, session := range []int64{0, 1_000_000} {
			want := max(wantPrompt, (textBytes(body)*2+4)/5, session)
			if got := estimateInputTokens(body, session); got != want {
				t.Fatalf("case %d input estimate=%d want %d", index, got, want)
			}
		}
	}
}

func BenchmarkEstimateInputTokens(b *testing.B) {
	for name, text := range map[string]string{
		"ASCII": strings.Repeat("x", 128<<10),
		"Mixed": strings.Repeat("code 中文textかな", 8192),
		"Image": `"data:image/png;base64,` + strings.Repeat("A", 128<<10) + `"`,
	} {
		b.Run(name, func(b *testing.B) {
			body := []byte(text)
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				estimateInputTokens(body, 0)
			}
		})
	}
}

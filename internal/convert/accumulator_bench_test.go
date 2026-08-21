package convert

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// BenchmarkXMLToolCallExtractor measures the streaming extractor's per-chunk
// allocation cost. The plain-text fast path should be allocation-free; the
// block cases used to allocate a fresh string per fragment (buffered += rest)
// and now append into a pooled buffer (issue #165).
func BenchmarkXMLToolCallExtractor(b *testing.B) {
	run := func(b *testing.B, frags []string) {
		b.Helper()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var x XMLToolCallExtractor
			for _, f := range frags {
				_, _ = x.Feed(f)
			}
			_, _ = x.Flush()
		}
	}

	b.Run("plain-text-fast-path", func(b *testing.B) {
		run(b, []string{
			"Here is a regular sentence without any tool call markers.",
			"Another ordinary fragment, still no opener anywhere.",
			"The stream keeps going with plain prose.",
			"Final text fragment before the end of the stream.",
		})
	})

	b.Run("block-across-fragments", func(b *testing.B) {
		run(b, []string{
			"I will run a command:\n<tool_call>",
			"\n<function=bash>",
			"\n<parameter=command>printf 'hello'</parameter>",
			"\n<parameter=cwd>/tmp</parameter>",
			"\n</function>",
			"\n</tool_call>",
			"\nDone.",
		})
	})

	b.Run("fenced-json-block", func(b *testing.B) {
		run(b, []string{
			"Result:\n```json\n",
			"{\"name\": \"bash\", \"arguments\": {\"command\": \"ls -la\"}}",
			"\n```",
		})
	})

	b.Run("many-blocks", func(b *testing.B) {
		run(b, []string{
			"first:\n<tool_call>{\"name\":\"a\",\"arguments\":{}}</tool_call>\n",
			"second:\n<codebuff_tool_call>{\"name\":\"b\",\"arguments\":{}}</codebuff_tool_call>\n",
			"third:\n<|tool_call_start|>\n<function=bash>\n<parameter=command>echo hi</parameter>\n</function>\n<|tool_call_end|>",
		})
	})

	b.Run("large-block-many-fragments", func(b *testing.B) {
		// A tool call whose arguments stream across 100 small fragments —
		// the case that used to concatenate a fresh string per fragment
		// (N allocs, O(N²) copying) and now appends into a pooled buffer.
		frags := make([]string, 0, 102)
		frags = append(frags, "Running:\n<tool_call>\n<function=bash>\n<parameter=command>")
		payload := strings.Repeat("a", 60)
		for i := 0; i < 100; i++ {
			frags = append(frags, payload)
		}
		frags = append(frags, "</parameter>\n</function>\n</tool_call>")
		run(b, frags)
	})
}

// TestXMLStreamExtractorPoolConcurrent exercises many extractors borrowing
// and releasing pooled buffers in parallel. Each stream must see only its own
// bytes — cross-stream corruption (a buffer handed to two extractors at once)
// shows up as wrong text or calls. `go test -race` additionally validates the
// pool is race-free.
func TestXMLStreamExtractorPoolConcurrent(t *testing.T) {
	frags := []string{
		"Let me check:\n<tool_call>",
		"\n<function=bash>",
		"\n<parameter=command>pwd</parameter>",
		"\n</function>\n</tool_call>",
		"\nDone.",
	}
	wantText := "Let me check:\n\nDone."
	wantArgs := `{"command":"pwd"}`

	// Serial reference: the exact output every worker must reproduce.
	var refText strings.Builder
	var refCalls []*toolCall
	{
		var x XMLToolCallExtractor
		for _, f := range frags {
			tt, cc := x.Feed(f)
			refText.WriteString(tt)
			refCalls = append(refCalls, cc...)
		}
		ft, fc := x.Flush()
		refText.WriteString(ft)
		refCalls = append(refCalls, fc...)
	}
	if refText.String() != wantText || len(refCalls) != 1 || refCalls[0].Function.Arguments != wantArgs {
		t.Fatalf("reference output wrong: text=%q calls=%d args=%q", refText.String(), len(refCalls), refCalls[0].Function.Arguments)
	}

	const workers = 8
	const iters = 200
	errs := make(chan string, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				var x XMLToolCallExtractor
				var text strings.Builder
				var calls []*toolCall
				for _, f := range frags {
					tt, cc := x.Feed(f)
					text.WriteString(tt)
					calls = append(calls, cc...)
				}
				ft, fc := x.Flush()
				text.WriteString(ft)
				calls = append(calls, fc...)
				if text.String() != wantText || len(calls) != 1 || calls[0].Function.Arguments != wantArgs {
					errs <- fmt.Sprintf("worker %d iter %d: text=%q calls=%d args=%q", w, i, text.String(), len(calls), calls[0].Function.Arguments)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

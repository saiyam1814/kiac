package main

import (
	"io"
	"strings"
	"testing"
)

func TestCheckInference(t *testing.T) {
	for _, tc := range []struct{ name, response, wantError string }{
		{"early-eos", `{"content":"GPU inference works","tokens_predicted":9,"stop_type":"eos"}`, ""},
		{"maximum", `{ "content": "GPU inference works", "tokens_predicted": 16, "stop_type": "limit", "timings": {} }`, ""},
		{"minimum", `{"content":"ok","tokens_predicted":1}`, ""},
		{"zero", `{"content":"text","tokens_predicted":0,"stop_type":"eos"}`, "tokens_predicted=0"},
		{"negative", `{"content":"text","tokens_predicted":-1}`, "expected 1..16"},
		{"above-maximum", `{"content":"text","tokens_predicted":17}`, "tokens_predicted=17"},
		{"missing-count", `{"content":"text"}`, "missing integer tokens_predicted"},
		{"null-count", `{"content":"text","tokens_predicted":null}`, "missing integer tokens_predicted"},
		{"fractional-count", `{"content":"text","tokens_predicted":1.5}`, "invalid inference response JSON"},
		{"string-count", `{"content":"text","tokens_predicted":"9"}`, "invalid inference response JSON"},
		{"missing-content", `{"tokens_predicted":9}`, "empty content"},
		{"empty-content", `{"content":" \n\t ","tokens_predicted":9}`, "empty content"},
		{"bad-json", `{"content":`, "invalid inference response JSON"},
		{"trailing-object", `{"content":"ok","tokens_predicted":9} {}`, "exactly one JSON object"},
		{"trailing-junk", `{"content":"ok","tokens_predicted":9} trailing`, "exactly one JSON object"},
		{"array", `[{"content":"ok","tokens_predicted":9}]`, "invalid inference response JSON"},
		{"null", `null`, "missing integer tokens_predicted"},
		{"error-response", `{"error":{"message":"model failed"}}`, "missing integer tokens_predicted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkInference(strings.NewReader(tc.response), 16)
			if tc.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error=%v, want %q", err, tc.wantError)
			}
		})
	}
}

func TestRunUsesRequestedMaximum(t *testing.T) {
	response := `{"content":"text","tokens_predicted":9,"stop_type":"eos"}`
	if err := run([]string{"--max-tokens", "9"}, strings.NewReader(response), io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--max-tokens", "8"}, strings.NewReader(response), io.Discard); err == nil || !strings.Contains(err.Error(), "tokens_predicted=9 (stop_type=\"eos\"); expected 1..8") {
		t.Fatalf("error=%v", err)
	}
	for _, args := range [][]string{{"--max-tokens", "0"}, {"--max-tokens", "invalid"}, {"extra"}} {
		if err := run(args, strings.NewReader(response), io.Discard); err == nil {
			t.Fatalf("accepted arguments %q", args)
		}
	}
}
